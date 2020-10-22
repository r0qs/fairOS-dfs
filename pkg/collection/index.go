package collection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fairdatasociety/fairOS-dfs/pkg/account"
	"github.com/fairdatasociety/fairOS-dfs/pkg/blockstore"
	"github.com/fairdatasociety/fairOS-dfs/pkg/feed"
	"github.com/fairdatasociety/fairOS-dfs/pkg/utils"
)

const (
	LeafEntry         = "L"
	IntermediateEntry = "I"
)

type Index struct {
	name        string
	user        utils.Address
	accountInfo *account.Info
	feed        *feed.API
	client      blockstore.Client
	count       uint64
	memDB       *Manifest
}

func CreateIndex(collectionName, IndexName string, fd *feed.API, user utils.Address) error {
	indexName := utils.PathSeperator + collectionName + utils.PathSeperator + IndexName
	topic := utils.HashString(indexName)
	_, _, err := fd.GetFeedData(topic, user)
	if err == nil {
		return ErrIndexAlreadyPresent
	}

	manifest := NewManifest(indexName, time.Now().Unix())

	// marshall and store the manifest as new feed
	data, err := json.Marshal(manifest)
	if err != nil {
		return ErrManifestUnmarshall
	}

	_, err = fd.CreateFeed(topic, user, data)
	if err != nil {
		return ErrManifestCreate
	}
	return nil
}

func OpenIndex(collectionName, IndexName string, fd *feed.API, ai *account.Info, user utils.Address, client blockstore.Client) (*Index, error) {
	idx := &Index{
		name:        utils.PathSeperator + collectionName + utils.PathSeperator + IndexName,
		user:        user,
		accountInfo: ai,
		feed:        fd,
		client:      client,
		count:       0,
		memDB:       nil,
	}

	err := idx.syncIndex()
	if err != nil {
		return nil, err
	}

	return idx, nil
}

func (idx *Index) DeleteIndex() error {
	// erase the top manifest
	topic := utils.HashString(idx.name)
	_, err := idx.feed.UpdateFeed(topic, idx.user, []byte(""))
	if err != nil {
		return ErrDeleteingIndex
	}
	return nil
}

func (idx *Index) Put(key string, refValue []byte) error {
	// get the first feed of the Index
	topic := utils.HashString(idx.name)
	_, data, err := idx.feed.GetFeedData(topic, idx.user)
	if err != nil {
		return ErrNoManifestFound
	}

	// unmarshall the manifest
	var manifest Manifest
	err = json.Unmarshal(data, &manifest)
	if err != nil {
		return err
	}

	ctx := context.Background()
	err = idx.addOrUpdateEntry(ctx, &manifest, key, refValue, false)

	idx.Print()

	return err

}

func (idx *Index) Print() error {
	manifest, err := idx.loadManifest(idx.name)
	if err != nil {
		return err
	}
	return  idx.PrintManifest(manifest, "---")
}

func (idx *Index) PrintManifest(manifest *Manifest, space string) error {
	for _, entry := range manifest.Entries {
		if entry.EType == LeafEntry {
			actualKey := manifest.Name + utils.PathSeperator + entry.Name
			actualKey = strings.TrimPrefix(actualKey, idx.name)
			actualKey = strings.Replace(actualKey, utils.PathSeperator, "", -1)
			fmt.Println( space + entry.Name + ":" + string(idx.getValue(entry.Ref)))
		}

		if entry.EType == IntermediateEntry {
			actualKey := manifest.Name + utils.PathSeperator + entry.Name
			actualKey = strings.TrimPrefix(actualKey, idx.name)
			actualKey = strings.Replace(actualKey, utils.PathSeperator, "", -1)
			fmt.Println(space + actualKey + ":" + entry.EType)
			newManifest, err := idx.loadManifest(manifest.Name + utils.PathSeperator + entry.Name)
			if err != nil {
				return err
			}
			idx.PrintManifest(newManifest, space )
		}
	}
	return nil
}

func (idx *Index) getValue(ref []byte) []byte {
	data, respCode, err := idx.client.DownloadBlob(ref)
	if err != nil {
		return nil
	}
	if respCode != http.StatusOK {
		return nil
	}
	return data
}

func (idx *Index) Get(key string) ([]byte, error) {
	_, manifest, i, err := idx.seekManifestAndEntry(key)
	if err != nil {
		return nil, err
	}
	return manifest.Entries[i].Ref, nil
}

func (idx *Index) Delete(key string) ([]byte, error) {
	_, manifest, i, err := idx.seekManifestAndEntry(key)
	if err != nil {
		return nil, err
	}

	deletedRef := manifest.Entries[i].Ref

	if len(manifest.Entries) == 1 && manifest.Entries[0].Name == "" {
		// then we have to remove the intermediate node in the parent manifest
		// so that the entire branch goes kaboom
		parentEntryKey := filepath.Base(manifest.Name)
		parentManifest, err := idx.loadManifest(filepath.Dir(manifest.Name))
		if err != nil {
			return nil, err
		}
		for i, entry := range parentManifest.Entries {
			if entry.EType == IntermediateEntry && entry.Name == parentEntryKey {
				deletedRef = entry.Ref
				parentManifest.Entries = append(parentManifest.Entries[:i], parentManifest.Entries[i+1:]...)
				break
			}
		}
		err = idx.updateManifest(parentManifest)
		if err != nil {
			return nil, err
		}
		return deletedRef, nil
	}

	manifest.Entries = append(manifest.Entries[:i], manifest.Entries[i+1:]...)
	err = idx.updateManifest(manifest)
	if err != nil {
		return nil, err
	}
	return deletedRef, nil
}

func (idx *Index) syncIndex() error {
	parentManifest, err := idx.loadManifest(idx.name)
	if err != nil {
		return err
	}

	if len(parentManifest.Entries) == 0 {
		return nil
	}

	errC := make(chan error, 1) // get only one error
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx.loadIndexInMemory(ctx, cancel, parentManifest, errC)
	select {
	case err := <-errC:
		if err != nil {
			idx.count = 0
			return err
		}
	default: // Default is must to avoid blocking
	}

	idx.memDB = parentManifest
	return nil
}

func (idx *Index) loadIndexInMemory(ctx context.Context, cancel context.CancelFunc, manifest *Manifest, errC chan error) {
	var count uint64
	var wg sync.WaitGroup

	for _, entry := range manifest.Entries {
		if entry.EType == IntermediateEntry {
			wg.Add(1)
			go func(ent *Entry) {
				defer wg.Done()
				newManifest, err := idx.loadManifest(manifest.Name + utils.PathSeperator + ent.Name)
				if err != nil {
					fmt.Println("error loading manifest ", manifest.Name+utils.PathSeperator+ent.Name, ent.EType)
					select {
					case errC <- err:
					default: // Default is must to avoid blocking
					}
					cancel()
					return
				}

				// if some other goroutine fails, terminate this one too
				select {
				case <-ctx.Done():
					return
				default: // Default is must to avoid blocking
				}
				entry.manifest = newManifest
				idx.loadIndexInMemory(ctx, cancel, newManifest, errC)
			}(entry)
		} else {
			count++
		}
	}
	wg.Wait()
	atomic.AddUint64(&idx.count, count)
}

func (idx *Index) addOrUpdateEntry(ctx context.Context, manifest *Manifest, key string, value []byte, memory bool) error {
	// go through the manifest to find the key
	manifest.dirtyFlag = false
	for i := range manifest.Entries {
		if key == "" {
			break
		}
		entry := manifest.Entries[i] // we change the entry so dont simplify this
		if entry.EType == LeafEntry {

			// an entry with the same key is already present... so update it
			if entry.Name == key {
				entry.Ref = value
				manifest.dirtyFlag = true
				break
			}

			prefix := longestCommonPrefix(key, entry.Name)
			if prefix != "" {
				// the new element is a prefix of the existing leaf..
				// add a new branch with two new leafs
				var newManifest Manifest
				newManifest.Name = manifest.Name + utils.PathSeperator + prefix
				newManifest.CreationTime = time.Now().Unix()
				entry1 := &Entry{
					Name:  strings.TrimPrefix(key, prefix),
					EType: LeafEntry,
					Ref:   value,
				}
				idx.addEntryToManifestSortedLexicographically(&newManifest, entry1)
				entry2 := &Entry{
					Name:  strings.TrimPrefix(entry.Name, prefix),
					EType: LeafEntry,
					Ref:   entry.Ref,
				}
				idx.addEntryToManifestSortedLexicographically(&newManifest, entry2)

				if !memory {
					// create a new feed with this new manifest
					data, err := json.Marshal(newManifest)
					if err != nil {
						return err
					}
					prefixTopic := utils.HashString(newManifest.Name)
					_, err = idx.feed.CreateFeed(prefixTopic, idx.user, data)
					if err != nil {
						return err
					}
				} else {
					entry.manifest = &newManifest
				}
				// convert the existing leaf to intermediate node
				entry.Name = prefix
				entry.EType = IntermediateEntry
				manifest.dirtyFlag = true
				break
			}
		} else {
			// go inside the branch and search
			if entry.EType == IntermediateEntry && strings.HasPrefix(key, entry.Name){
			    newKey := strings.TrimPrefix(key, entry.Name)
				topic := utils.HashString(manifest.Name + utils.PathSeperator + entry.Name)

				_, data, err := idx.feed.GetFeedData(topic, idx.user)
				if err != nil {
					return err
				}
				var intermediateManifest Manifest
				err = json.Unmarshal(data, &intermediateManifest)
				if err != nil {
					return err
				}
				return idx.addOrUpdateEntry(ctx, &intermediateManifest, newKey, value, memory)
			} else if entry.EType == IntermediateEntry && strings.HasPrefix(entry.Name, key) {
				// break the branch in to more deeper branch
				// abc-> "" , d
				// ab -> "", c -> "", d
				prefix := key
				newKey := strings.TrimPrefix(entry.Name, key)

				// load the abc manifest
				topic := utils.HashString(manifest.Name + utils.PathSeperator + entry.Name)
				_, data, err := idx.feed.GetFeedData(topic, idx.user)
				if err != nil {
					return err
				}
				var existingManifest Manifest
				err = json.Unmarshal(data, &existingManifest)
				if err != nil {
					return err
				}
				existingManifest.Name = strings.TrimSuffix(existingManifest.Name, prefix+newKey) + prefix + utils.PathSeperator + newKey
				err = idx.updateManifest(&existingManifest)
				if err != nil {
					return err
				}

				// create the new manifest branch
				var newManifest Manifest
				newManifest.Name = manifest.Name + utils.PathSeperator + key
				newManifest.CreationTime = time.Now().Unix()
				entry1 := &Entry{
					Name:  "",
					EType: LeafEntry,
					Ref:   value,
				}
				idx.addEntryToManifestSortedLexicographically(&newManifest, entry1)
				entry2 := &Entry{
					Name:  newKey,
					EType: IntermediateEntry,
					manifest: &existingManifest,
				}
				idx.addEntryToManifestSortedLexicographically(&newManifest, entry2)
				if !memory {
					// create a new feed with this new manifest
					data, err := json.Marshal(newManifest)
					if err != nil {
						return err
					}
					prefixTopic := utils.HashString(newManifest.Name)
					_, err = idx.feed.CreateFeed(prefixTopic, idx.user, data)
					if err != nil {
						return err
					}
				} else {
					entry.manifest = &newManifest
				}

				// convert the existing intermediate node to a reduced branch node
				// make abc to ab
				entry.Name = prefix
				entry.EType = IntermediateEntry
				manifest.dirtyFlag = true
				break
			}
		}
	}

	// if the manifest is not already changed, then this is a new entry
	if !manifest.dirtyFlag {
		newEntry := Entry{
			Name:  key,
			EType: LeafEntry,
			Ref:   value,
		}
		idx.addEntryToManifestSortedLexicographically(manifest, &newEntry)
		manifest.dirtyFlag = true
	}

	if manifest.dirtyFlag && !memory {
		return idx.updateManifest(manifest)
	}
	return nil
}

func (idx *Index) addEntryToManifestSortedLexicographically(manifest *Manifest, entryToAdd *Entry) {
	var entries []*Entry

	// this is the first element
	if len(manifest.Entries) == 0 {
		manifest.Entries = append(manifest.Entries, entryToAdd)
		return
	}

	// new element has an empty name, so add it in the beginning
	if len(entryToAdd.Name) == 0 {
		entries = append(entries, entryToAdd)
		manifest.Entries = append(entries, manifest.Entries...)
		return
	}

	entryAdded := false
	for _, entry := range manifest.Entries {
		if len(entry.Name) == 0 {
			entries = append(entries, entry)
			continue
		} else {
			if !entryAdded {
				a := entry.Name[0]
				b := entryToAdd.Name[0]
				if a > b {
					entries = append(entries, entryToAdd)
					entryAdded = true
				}
			}
			entries = append(entries, entry)
		}
	}

	if !entryAdded {
		entries = append(entries, entryToAdd)
	}

	manifest.Entries = entries
}

func (idx *Index) seekManifestAndEntry(key string) (*Manifest, *Manifest, int, error) {
	// load the first manifest of the index
	firstManifest, err := idx.loadManifest(idx.name)
	if err != nil && !errors.Is(err, ErrNoManifestFound) {
		return nil, nil, 0, err
	}

	// if there are any elements in the index, then search for the entry
	if len(firstManifest.Entries) > 0 {
		return idx.findManifest(nil, firstManifest, key, false)
	}
	return nil, nil, 0, ErrEntryNotFound
}

func (idx *Index) findManifest(grandParentManifest, parentManifest *Manifest, key string, memory bool) (*Manifest, *Manifest, int, error) {
	for i, entry := range parentManifest.Entries {

		// if the first char is > keys first char, then the key wont be found
		if len(entry.Name) > 0 {
			if len(key) == 0 { // to check for empty entry
				return nil, nil, 0, ErrEntryNotFound
			}
			if entry.Name[0] > key[0] { // to check for greater entries
				return nil, parentManifest, 0, ErrEntryNotFound
			}
		}

		if entry.EType == LeafEntry && entry.Name == key {
			return grandParentManifest, parentManifest, i, nil
		}

		if entry.EType == IntermediateEntry && strings.HasPrefix(key, entry.Name) {
			childManifestPath := parentManifest.Name + utils.PathSeperator + entry.Name
			childManifest, err := idx.loadManifest(childManifestPath)
			if err != nil {
				return nil, nil, 0, err
			}
			if memory {
				entry.manifest = childManifest
			}
			childKey := strings.TrimPrefix(key, entry.Name)
			return idx.findManifest(parentManifest, childManifest, childKey, memory)
		}
	}
	return nil, nil, 0, ErrEntryNotFound
}

func (idx *Index) loadManifest(manifestPath string) (*Manifest, error) {
	// get feed data and unmarshall the manifest
	topic := utils.HashString(manifestPath)
	_, data, err := idx.feed.GetFeedData(topic, idx.user)
	if err != nil {
		return nil, ErrNoManifestFound
	}

	var manifest Manifest
	err = json.Unmarshal(data, &manifest)
	if err != nil {
		return nil, ErrManifestUnmarshall
	}
	return &manifest, nil
}

func (idx *Index) updateManifest(manifest *Manifest) error {
	// marshall and update the manifest in the feed
	data, err := json.Marshal(manifest)
	if err != nil {
		return ErrManifestUnmarshall
	}

	topic := utils.HashString(manifest.Name)
	_, err = idx.feed.UpdateFeed(topic, idx.user, data)
	if err != nil {
		return ErrManifestCreate
	}
	return nil
}

func (idx *Index) storeManifest(manifest *Manifest) error {
	// marshall and store the manifest as new feed
	data, err := json.Marshal(manifest)
	if err != nil {
		return ErrManifestUnmarshall
	}

	topic := utils.HashString(manifest.Name)
	_, err = idx.feed.CreateFeed(topic, idx.user, data)
	if err != nil {
		return ErrManifestCreate
	}
	return nil
}

type Iterator struct {
	index         *Index
	startPrefix   string
	endPrefix     string
	limit         int64
	givenUntilNow int64
	currentKey    string
	currentValue  []byte
	manifestStack []*ManifestState
	error         error
}

type ManifestState struct {
	currentManifest *Manifest
	currentIndex    int
}

func (idx *Index) NewIterator(start, end string, limit int64) (*Iterator, error) {
	// get the first feed of the Index
	topic := utils.HashString(idx.name)
	_, data, err := idx.feed.GetFeedData(topic, idx.user)
	if err != nil {
		return nil, ErrEmptyIndex
	}

	var manifest Manifest
	err = json.Unmarshal(data, &manifest)
	if err != nil {
		return nil, err
	}

	firstManifest := &ManifestState{
		currentManifest: &manifest,
		currentIndex:    0,
	}
	var stack []*ManifestState
	stack = append(stack, firstManifest)

	itr := &Iterator{
		index:         idx,
		startPrefix:   start,
		endPrefix:     end,
		limit:         limit,
		givenUntilNow: 0,
		manifestStack: stack,
		currentKey:    "",
		currentValue:  nil,
		error:         nil,
	}

	if itr.startPrefix != "" {
		err := itr.Seek(itr.startPrefix)
		if err != nil {
			return nil, err
		}
	}

	return itr, nil
}



func (itr *Iterator) Next() bool {
	// dont go beyond the limit
	if itr.limit > 0 {
		if itr.givenUntilNow >= itr.limit {
			return false
		}
	}

	// get the current manifest at the top of the stack
	depthOfStack := len(itr.manifestStack)
	if depthOfStack == 0 {
		itr.error = ErrNoNextElement
		return false
	}

	// take the top manifest to find the next entry
	manifestState := itr.manifestStack[depthOfStack-1]

	entriesExhausted := true
	for entriesExhausted {
		// see if we have exhausted the entries in the current manifest
		if manifestState.currentIndex >= len(manifestState.currentManifest.Entries) {
			// pop the exhausted manifest from the top and pick the next manifest to find the entry
			n := depthOfStack - 1
			if n == 0 {
				itr.error = ErrNoNextElement
				return false
			}
			manifestState = itr.manifestStack[n-1]
			itr.manifestStack[n] = nil
			itr.manifestStack = itr.manifestStack[:n]
			depthOfStack = n
		} else {
			entriesExhausted = false
		}
	}

	// We have a manifest whose entries are not yet exhausted,
	// so get the next entry and check for valid conditions of the Iterator()
	entry := manifestState.currentManifest.Entries[manifestState.currentIndex]
	manifestState.currentIndex++

	// check if the search has reached the end key
	if itr.endPrefix != "" {
		actualKey := manifestState.currentManifest.Name + utils.PathSeperator + entry.Name
		actualKey = strings.TrimPrefix(actualKey, itr.index.name)
		actualKey = strings.Replace(actualKey, utils.PathSeperator, "", -1)
		if actualKey[0] > itr.endPrefix[0] {
			return false
		}
	}

	// if it is a leaf entry, set the key and value
	if entry.EType == LeafEntry {
		actualKey := manifestState.currentManifest.Name + utils.PathSeperator + entry.Name
		actualKey = strings.TrimPrefix(actualKey, itr.index.name)
		actualKey = strings.Replace(actualKey, utils.PathSeperator, "", -1)
		itr.currentKey = actualKey
		itr.currentValue = entry.Ref
		itr.givenUntilNow++
		return true
	}

	// if it is an intermediate entry, get the branch manifest and push in to the stack
	if entry.EType == IntermediateEntry {
		topic := utils.HashString(manifestState.currentManifest.Name + utils.PathSeperator + entry.Name)
		_, data, err := itr.index.feed.GetFeedData(topic, itr.index.user)
		if err != nil {
			itr.error = err
			return false
		}

		var newManifest Manifest
		err = json.Unmarshal(data, &newManifest)
		if err != nil {
			itr.error = err
			return false
		}
		newManifestState := &ManifestState{
			currentManifest: &newManifest,
			currentIndex:    0,
		}
		itr.manifestStack = append(itr.manifestStack, newManifestState)

		//if entry.Name == "" {
		//	actualKey := manifestState.currentManifest.Name
		//	actualKey = strings.TrimPrefix(actualKey, itr.index.name)
		//	actualKey = strings.Replace(actualKey, utils.PathSeperator, "", -1)
		//	itr.currentKey = actualKey
		//	itr.currentValue = entry.Ref
		//	itr.givenUntilNow++
		//	return true
		//}

		return itr.Next()
	}
	return false
}

func (itr *Iterator) Key() string {
	return itr.currentKey
}

func (itr *Iterator) Value() []byte {
	return itr.currentValue
}

func (itr *Iterator) Seek(key string) error {
	manifest, err := itr.index.loadManifest(itr.index.name)
	if err != nil {
		return err
	}

	err = itr.seekKey(manifest, key)
	if err != nil {
		return err
	}
	return nil
}

func (itr *Iterator) seekKey(manifest *Manifest, key string) error {
	// if there are any elements in the index, then search for the entry
	if len(manifest.Entries) > 0 {
		for i, entry := range manifest.Entries {

			// even if the entry is not found, add the pointer to seek so that
			// seek can continue from the next element
			if len(entry.Name) > 0 {
				if len(key) == 0 || entry.Name[0] > key[0] {
					manifestState := &ManifestState{
						currentManifest: manifest,
						currentIndex:    i + 1,
					}
					itr.manifestStack = append(itr.manifestStack, manifestState)
					return ErrEntryNotFound
				}
			}

			if entry.EType == LeafEntry && entry.Name == key {
				manifestState := &ManifestState{
					currentManifest: manifest,
					currentIndex:    i,
				}
				itr.manifestStack = append(itr.manifestStack, manifestState)
				//actualKey := manifest.Name + utils.PathSeperator + entry.Name
				//actualKey = strings.TrimPrefix(actualKey, itr.index.name)
				//actualKey = strings.Replace(actualKey, utils.PathSeperator, "", -1)
				//
				//itr.currentKey = actualKey
				//itr.currentValue = entry.Ref
				//itr.givenUntilNow++
				return nil
			}

			if entry.EType == IntermediateEntry && strings.HasPrefix(key, entry.Name) {
				// found a branch, push the current manifest state
				manifestState := &ManifestState{
					currentManifest: manifest,
					currentIndex:    i + 1,
				}
				itr.manifestStack = append(itr.manifestStack, manifestState)

				// now load the child manifest and re-seek
				topic := utils.HashString(manifest.Name + utils.PathSeperator + entry.Name)
				_, data, err := itr.index.feed.GetFeedData(topic, itr.index.user)
				if err != nil {
					return err
				}
				var childManifest Manifest
				err = json.Unmarshal(data, &childManifest)
				if err != nil {
					return err
				}

				childKey := strings.TrimPrefix(key, entry.Name)
				return itr.seekKey(&childManifest, childKey)
			}
		}
	}
	return ErrEntryNotFound
}

type Batch struct {
	idx   *Index
	memDb *Manifest
}

func (idx *Index) Batch() (*Batch, error) {
	return &Batch{
		idx: idx,
	}, nil
}

func (b *Batch) Put(key string, refValue []byte) error {
	if b.memDb == nil {
		manifest := &Manifest{
			Name:      b.idx.name,
			dirtyFlag: true,
		}
		b.memDb = manifest
	}
	ctx := context.Background()
	return b.idx.addOrUpdateEntry(ctx, b.memDb, key, refValue, true)
}

func (b *Batch) Delete(key string) ([]byte, error) {
	if b.memDb == nil {
		return nil, ErrEntryNotFound
	}
	parentManifest, manifest, index, err := b.idx.findManifest(nil, b.memDb, key, true)
	if err != nil {
		return nil, err
	}

	deletedRef := manifest.Entries[index].Ref
	if len(manifest.Entries) == 1 && manifest.Entries[0].Name == "" && parentManifest != nil {
		// then we have to remove the intermediate node in the parent manifest
		// so that the entire branch goes kaboom
		parentEntryKey := filepath.Base(manifest.Name)
		for i, entry := range parentManifest.Entries {
			if entry.EType == IntermediateEntry && entry.Name == parentEntryKey {
				deletedRef = entry.Ref
				parentManifest.Entries = append(parentManifest.Entries[:i], parentManifest.Entries[i+1:]...)
				parentManifest.dirtyFlag = true
				break
			}
		}
		return deletedRef, nil
	}
	manifest.Entries = append(manifest.Entries[:index], manifest.Entries[index+1:]...)
	manifest.dirtyFlag = true
	return deletedRef, nil
}

func (b *Batch) Write() error {
	if b.memDb != nil {
		return ErrEntryNotFound
	}

	if b.memDb.dirtyFlag {
		diskManifest, err := b.idx.loadManifest(b.memDb.Name)
		if err != nil {
			return err
		}
		return b.mergeAndWriteManifest(diskManifest, b.memDb)
	}
	return nil
}

func (b *Batch) mergeAndWriteManifest(diskManifest, memManifest *Manifest) error {

	// if there is no disk equivalent, then just store the memory version
	if diskManifest == nil {
		return b.idx.storeManifest(memManifest)
	}

	// merge the mem manifest with the disk version
	if memManifest.dirtyFlag {
		for _, dirtyEntry := range memManifest.Entries {
			b.idx.addEntryToManifestSortedLexicographically(diskManifest, dirtyEntry)
			diskManifest.dirtyFlag = true
			if dirtyEntry.EType == IntermediateEntry && dirtyEntry.manifest != nil {
				err := b.storeMemoryManifest(dirtyEntry.manifest)
				if err != nil {
					return err
				}
			}
		}

		if diskManifest.dirtyFlag {
			// save th disk manifest
			err := b.idx.storeManifest(diskManifest)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Batch) storeMemoryManifest(manifest *Manifest) error {
	// store this manifest
	err := b.idx.storeManifest(manifest)
	if err != nil {
		return err
	}

	// store any branches in this manifest
	for _, entry := range manifest.Entries {
		if entry.EType == IntermediateEntry && entry.manifest != nil {
			err := b.storeMemoryManifest(entry.manifest)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func longestCommonPrefix(str1, str2 string) string {
	if str1 == "" || str2 == "" {
		return ""
	}
	maxLen := len(str2)
	if len(str1) < len(str2) {
		maxLen = len(str1)
	}

	matchLen := 0
	for i := 0; i < maxLen; i++ {
		if str1[i] != str2[i] {
			break
		} else {
			matchLen++
		}
	}
	if matchLen == 0 {
		return ""
	}
	return str1[:matchLen]
}
