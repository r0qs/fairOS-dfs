/*
Copyright © 2020 FairOS Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collection

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/fairdatasociety/fairOS-dfs/pkg/account"
	"github.com/fairdatasociety/fairOS-dfs/pkg/blockstore"
	"github.com/fairdatasociety/fairOS-dfs/pkg/common"
	"github.com/fairdatasociety/fairOS-dfs/pkg/feed"
	"github.com/fairdatasociety/fairOS-dfs/pkg/logging"
)

const (
	DocumentFile          = "document_dbs"
	DefaultIndexFieldName = "id"
	FieldTypeMap          = "map"
	FieldTypeList         = "list"
)

type Document struct {
	fd          *feed.API
	ai          *account.Info
	user        common.Address
	client      blockstore.Client
	openDocDBs  map[string]*DocumentDB
	openDOcDBMu sync.RWMutex
	logger      logging.Logger
}

type DocumentDB struct {
	name          string
	simpleIndexes map[string]*Index
	mapIndexes    map[string]*Index
	listIndexes   map[string]*Index
}

type DBSchema struct {
	Name            string   `json:"name"`
	SimpleIndexes   []SIndex `json:"simple_indexes,omitempty"`
	MapIndexes      []SIndex `json:"map_indexes,omitempty"`
	ListIndexes     []SIndex `json:"list_indexes,omitempty"`
	CompoundIndexes []CIndex `json:"compound_indexes,omitempty"`
}

type SIndex struct {
	FieldName string    `json:"name"`
	FieldType IndexType `json:"type"`
}

type CIndex struct {
	SimpleIndexes []SIndex
}

type DocBatch struct {
	db      *DocumentDB
	batches map[string]*Batch
}

func NewDocumentStore(fd *feed.API, ai *account.Info, user common.Address, client blockstore.Client, logger logging.Logger) *Document {
	return &Document{
		fd:         fd,
		ai:         ai,
		user:       user,
		client:     client,
		openDocDBs: make(map[string]*DocumentDB),
		logger:     logger,
	}
}

func (d *Document) CreateDocumentDB(dbName string, indexes map[string]IndexType) error {
	if d.fd.IsReadOnlyFeed() {
		return ErrReadOnlyIndex
	}

	// check if the db is already present and opened
	if d.IsDBOpened(dbName) {
		return ErrDocumentDBAlreadyOpened
	}

	// load the existing db's and see if this name is already there
	docTables, err := d.LoadDocumentDBSchemas()
	if err != nil {
		return err
	}
	if _, ok := docTables[dbName]; ok {
		return ErrDocumentDBAlreadyPresent
	}

	// since this db is not present already, create the table
	err = CreateIndex(dbName, DefaultIndexFieldName, StringIndex, d.fd, d.user, d.client)
	if err != nil {
		return err
	}

	var simpleIndexes []SIndex
	var mapIndexes []SIndex
	var listIndexes []SIndex

	// create the default index
	defaultIndex := SIndex{
		FieldName: DefaultIndexFieldName,
		FieldType: StringIndex,
	}
	simpleIndexes = append(simpleIndexes, defaultIndex)

	// Now add the other indexes to simpleIndexes array
	for fieldName, fieldType := range indexes {
		// create the simple index
		err = CreateIndex(dbName, fieldName, fieldType, d.fd, d.user, d.client)
		if err != nil {
			return err
		}
		newIndex := SIndex{
			FieldName: fieldName,
			FieldType: fieldType,
		}
		if fieldType == MapIndex {
			mapIndexes = append(mapIndexes, newIndex)
		} else if fieldType == ListIndex {
			listIndexes = append(listIndexes, newIndex)
		} else {
			simpleIndexes = append(simpleIndexes, newIndex)
		}
	}

	// add the simple indexes to the schema
	docTables[dbName] = DBSchema{
		Name:          dbName,
		SimpleIndexes: simpleIndexes,
		MapIndexes:    mapIndexes,
		ListIndexes:   listIndexes,
	}
	return d.storeDocumentDBSchemas(docTables)
}

func (d *Document) OpenDocumentDB(dbName string) error {
	// check if the db is already present and opened
	if d.IsDBOpened(dbName) {
		return ErrDocumentDBAlreadyOpened
	}

	// load the existing db's and see if this name is present
	docTables, err := d.LoadDocumentDBSchemas()
	if err != nil {
		return err
	}
	schema, ok := docTables[dbName]
	if !ok {
		return ErrDocumentDBNotPresent
	}

	// open the simple indexes
	simpleIndexs := make(map[string]*Index)
	for _, si := range schema.SimpleIndexes {
		idx, err := OpenIndex(dbName, si.FieldName, d.fd, d.ai, d.user, d.client, d.logger)
		if err != nil {
			return err
		}
		simpleIndexs[si.FieldName] = idx
	}

	// open the map indexes
	mapIndexs := make(map[string]*Index)
	for _, mi := range schema.MapIndexes {
		idx, err := OpenIndex(dbName, mi.FieldName, d.fd, d.ai, d.user, d.client, d.logger)
		if err != nil {
			return err
		}
		mapIndexs[mi.FieldName] = idx
	}

	// open the list indexes
	listIndexes := make(map[string]*Index)
	for _, li := range schema.MapIndexes {
		idx, err := OpenIndex(dbName, li.FieldName, d.fd, d.ai, d.user, d.client, d.logger)
		if err != nil {
			return err
		}
		listIndexes[li.FieldName] = idx
	}

	// create the document DB index map
	docDB := &DocumentDB{
		name:          dbName,
		simpleIndexes: simpleIndexs,
		mapIndexes:    mapIndexs,
		listIndexes:   listIndexes,
	}

	// add to the open DB map
	d.addToOpenedDb(dbName, docDB)
	return nil
}

func (d *Document) DeleteDocumentDB(dbName string) error {
	if d.fd.IsReadOnlyFeed() {
		return ErrReadOnlyIndex
	}

	// load the existing db's and see if this name is already there
	docTables, err := d.LoadDocumentDBSchemas()
	if err != nil {
		return err
	}

	// check if the table exists before deleting
	_, found := docTables[dbName]
	if !found {
		return ErrDocumentDBNotPresent
	}

	// open and delete the indexes
	if !d.IsDBOpened(dbName) {
		return d.OpenDocumentDB(dbName)
	}
	docDB := d.getOpenedDb(dbName)
	// TODO: before deleting the indexes, unpin all the documents referenced in the ID index
	for _, si := range docDB.simpleIndexes {
		return si.DeleteIndex()
	}
	for _, mi := range docDB.mapIndexes {
		return mi.DeleteIndex()
	}
	for _, li := range docDB.listIndexes {
		return li.DeleteIndex()
	}

	// delete the document db from the DB file
	delete(docTables, dbName)

	// store the rest of the document db
	return d.storeDocumentDBSchemas(docTables)
}

func (d *Document) Count(dbName, expr string) (uint64, error) {
	db := d.getOpenedDb(dbName)
	if db == nil {
		return 0, ErrDocumentDBNotOpened
	}

	// count all documents
	if expr == "" {
		idx, found := db.simpleIndexes[DefaultIndexFieldName]
		if !found {
			return 0, ErrIndexNotPresent
		}
		return idx.CountIndex()
	}

	// count documents based on expression
	fieldName, operator, fieldValue, err := d.resolveExpression(expr)
	if err != nil {
		return 0, err
	}
	idx, found := db.simpleIndexes[fieldName]
	if !found {
		idx, found = db.mapIndexes[fieldName]
		if !found {
			idx, found = db.listIndexes[fieldName]
			if !found {
				return 0, ErrIndexNotPresent
			}
		} else {
			fieldValue = strings.ReplaceAll(fieldValue, ":", "")
		}
	}

	switch idx.indexType {
	case StringIndex, MapIndex, ListIndex:
		itr, err := idx.NewStringIterator(fieldValue, "", -1)
		if err != nil {
			return 0, err
		}
		switch operator {
		case "=":
			itr.Next()
			refs := itr.ValueAll()
			return uint64(len(refs)), nil
		case "=>":
			var count uint64
			for itr.Next() {
				refs := itr.ValueAll()
				count = count + uint64(len(refs))
			}
			return count, nil
		case ">":
			var count uint64
			for itr.Next() {
				if itr.StringKey() == fieldValue {
					continue
				}
				refs := itr.ValueAll()
				count = count + uint64(len(refs))
			}
			return count, nil
		}
	case NumberIndex:
		start, err := strconv.ParseInt(fieldValue, 10, 64)
		if err != nil {
			return 0, err
		}
		itr, err := idx.NewIntIterator(start, -1, -1)
		if err != nil {
			return 0, err
		}
		switch operator {
		case "=":
			itr.Next()
			refs := itr.ValueAll()
			return uint64(len(refs)), nil
		case "=>":
			var count uint64
			for itr.Next() {
				refs := itr.ValueAll()
				count = count + uint64(len(refs))
			}
			return count, nil
		case ">":
			var count uint64
			for itr.Next() {
				if itr.IntegerKey() == start {
					continue
				}
				refs := itr.ValueAll()
				count = count + uint64(len(refs))
			}
			return count, nil
		}
	case BytesIndex:
		return 0, ErrIndexNotSupported
	default:
		return 0, ErrInvalidIndexType
	}
	return 0, nil
}

func (d *Document) Put(dbName string, doc []byte) error {
	if d.fd.IsReadOnlyFeed() {
		return ErrReadOnlyIndex
	}

	db := d.getOpenedDb(dbName)
	if db == nil {
		return ErrDocumentDBNotOpened
	}

	var t interface{}
	err := json.Unmarshal(doc, &t)
	if err != nil {
		return err
	}
	docMap := t.(map[string]interface{})

	// check if docMap has all the fields in the simpleIndex
	for field := range db.simpleIndexes {
		if _, found := docMap[field]; !found {
			return ErrDocumentDBIndexFieldNotPresent
		}
	}

	// check if the id is already present
	// and remove it if it is present
	idValue := docMap[DefaultIndexFieldName]
	switch v := idValue.(type) {
	case string:
		if v == "" {
			return ErrInvalidDocumentId
		} else {
			idIndex := db.simpleIndexes[DefaultIndexFieldName]
			refs, err := idIndex.Get(v)
			if err != nil {
				break
			}
			if len(refs) > 0 {
				err = d.Del(dbName, v)
				if err != nil {
					return err
				}
			}
		}
	default:
		return ErrInvalidIndexType
	}

	// upload the document
	ref, err := d.client.UploadBlob(doc, true, true)
	if err != nil {
		return err
	}

	// update the indexes
	indexes := make(map[string]*Index)
	for field, index := range db.simpleIndexes {
		indexes[field] = index
	}
	for field, index := range db.mapIndexes {
		indexes[field] = index
	}
	for field, index := range db.listIndexes {
		indexes[field] = index
	}
	for field, index := range indexes {
		v := docMap[field] // it is already checked to be present
		switch index.indexType {
		case StringIndex:
			apnd := true
			if field == DefaultIndexFieldName {
				apnd = false
			}
			err := index.Put(v.(string), ref, StringIndex, apnd)
			if err != nil {
				return err
			}
		case MapIndex:
			valMap := v.(map[string]interface{})
			for keyField, vf := range valMap {
				valueField := vf.(string)
				mapField := keyField + valueField
				err := index.Put(mapField, ref, StringIndex, true)
				if err != nil {
					return err
				}
			}
		case ListIndex:
			valList := v.([]string)
			for _, listVal := range valList {
				err := index.Put(listVal, ref, StringIndex, true)
				if err != nil {
					return err
				}
			}
		case NumberIndex:
			val := v.(float64)
			val1 := int64(val)
			valStr := strconv.FormatInt(val1, 10)
			err := index.Put(valStr, ref, NumberIndex, true)
			if err != nil {
				return err
			}
		case BytesIndex:
			return ErrIndexNotSupported
		default:
			return ErrInvalidIndexType
		}
	}
	return nil
}

func (d *Document) Get(dbName, id string) ([]byte, error) {
	db := d.getOpenedDb(dbName)
	if db == nil {
		return nil, ErrDocumentDBNotOpened
	}

	idIndex := db.simpleIndexes[DefaultIndexFieldName]
	references, err := idIndex.Get(id)
	if err != nil {
		return nil, err
	}

	if len(references) == 0 {
		return nil, ErrDocumentNotPresent
	}

	data, _, err := d.client.DownloadBlob(references[0])
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (d *Document) Del(dbName, id string) error {
	if d.fd.IsReadOnlyFeed() {
		return ErrReadOnlyIndex
	}

	db := d.getOpenedDb(dbName)
	if db == nil {
		return ErrDocumentDBNotOpened
	}

	// get the "id" index and retrieve the original document
	idx := db.simpleIndexes[DefaultIndexFieldName]
	refs, err := idx.Get(id)
	if err != nil {
		if errors.Is(err, ErrEntryNotFound) {
			return nil
		}
		return err
	}
	if len(refs) <= 0 {
		return nil
	}

	data, _, err := d.client.DownloadBlob(refs[0])
	if err != nil {
		return err
	}

	var t interface{}
	err = json.Unmarshal(data, &t)
	if err != nil {
		return err
	}
	docMap := t.(map[string]interface{})

	// delete all the indexes of the doc
	for field, index := range db.simpleIndexes {
		v := docMap[field] // it is already checked to be present
		switch index.indexType {
		case StringIndex:
			_, err := index.Delete(v.(string))
			if err != nil {
				return err
			}
		case MapIndex:
			valMap := v.(map[string]interface{})
			for keyField, valueField := range valMap {
				vf := valueField.(string)
				mapField := keyField + vf
				_, err := index.Delete(mapField)
				if err != nil {
					return err
				}
			}
		case ListIndex:
			valList := v.([]string)
			for _, listVal := range valList {
				_, err := index.Delete(listVal)
				if err != nil {
					return err
				}
			}

		case NumberIndex:
			val := v.(float64)
			val1 := int64(val)
			valStr := strconv.FormatInt(val1, 10)
			_, err := index.Delete(valStr)
			if err != nil {
				return err
			}
		case BytesIndex:
			return ErrIndexNotSupported
		default:
			return ErrInvalidIndexType
		}
	}

	// delete the original data (unpin)
	return d.client.DeleteBlob(refs[0])
}

func (d *Document) Find(dbName, expr string, limit int) ([][]byte, error) {
	db := d.getOpenedDb(dbName)
	if db == nil {
		return nil, ErrDocumentDBNotOpened
	}

	fieldName, operator, fieldValue, err := d.resolveExpression(expr)
	if err != nil {
		return nil, err
	}

	idx, found := db.simpleIndexes[fieldName]
	if !found {
		idx, found = db.mapIndexes[fieldName]
		if !found {
			idx, found = db.listIndexes[fieldName]
			if !found {
				return nil, ErrIndexNotPresent
			}
		} else {
			fieldValue = strings.ReplaceAll(fieldValue, ":", "")
		}
	}

	var references [][]byte
	switch idx.indexType {
	case StringIndex, MapIndex, ListIndex:
		itr, err := idx.NewStringIterator(fieldValue, "", int64(limit))
		if err != nil {
			return nil, err
		}
		switch operator {
		case "=":
			itr.Next()
			references = itr.ValueAll()
		case "=>":
			for itr.Next() {
				if limit > 0 && len(references) > limit {
					break
				}
				refs := itr.ValueAll()
				references = append(references, refs...)
			}
		case ">":
			for itr.Next() {
				if limit > 0 && len(references) > limit {
					break
				}
				if itr.StringKey() == fieldValue {
					continue
				}
				refs := itr.ValueAll()
				references = append(references, refs...)
			}
		}
	case NumberIndex:
		start, err := strconv.ParseInt(fieldValue, 10, 64)
		if err != nil {
			return nil, err
		}
		itr, err := idx.NewIntIterator(start, -1, int64(limit))
		if err != nil {
			return nil, err
		}
		switch operator {
		case "=":
			itr.Next()
			references = itr.ValueAll()
		case "=>":
			for itr.Next() {
				if limit > 0 && len(references) > limit {
					break
				}
				refs := itr.ValueAll()
				references = append(references, refs...)
			}
		case ">":
			for itr.Next() {
				if limit > 0 && len(references) > limit {
					break
				}
				if itr.IntegerKey() == start {
					continue
				}
				refs := itr.ValueAll()
				references = append(references, refs...)
			}
		}
	case BytesIndex:
		return nil, ErrIndexNotSupported
	default:
		return nil, ErrInvalidIndexType
	}

	var docs [][]byte
	for _, ref := range references {
		if limit > 0 && len(docs) >= limit {
			break
		}
		data, _, err := d.client.DownloadBlob(ref)
		if err != nil {
			return nil, err
		}
		docs = append(docs, data)
	}
	return docs, nil
}

func (d *Document) LoadDocumentDBSchemas() (map[string]DBSchema, error) {
	collections := make(map[string]DBSchema)
	topic := common.HashString(DocumentFile)
	_, data, err := d.fd.GetFeedData(topic, d.user)
	if err != nil {
		if err.Error() != "no feed updates found" {
			return collections, err
		}
	}

	buf := bytes.NewBuffer(data)
	rd := bufio.NewReader(buf)
	for {
		line, err := rd.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("loading collections: %w", err)
		}
		line = strings.Trim(line, "\n")

		var schema DBSchema
		err = json.Unmarshal([]byte(line), &schema)
		if err != nil {
			return nil, ErrUnmarshallingDBSchema
		}
		collections[schema.Name] = schema
	}
	return collections, nil
}

func (d *Document) storeDocumentDBSchemas(collections map[string]DBSchema) error {
	buf := bytes.NewBuffer(nil)
	collectionLen := len(collections)
	if collectionLen > 0 {
		for _, schema := range collections {
			line, err := json.Marshal(schema)
			if err != nil {
				return ErrMarshallingDBSchema
			}
			buf.WriteString(string(line) + "\n")
		}
	}
	topic := common.HashString(DocumentFile)
	_, err := d.fd.UpdateFeed(topic, d.user, buf.Bytes())
	if err != nil {
		return err
	}
	return nil
}

func (d *Document) IsDBOpened(dbName string) bool {
	d.openDOcDBMu.Lock()
	defer d.openDOcDBMu.Unlock()
	if _, found := d.openDocDBs[dbName]; found {
		return true
	}
	return false
}

func (d *Document) getOpenedDb(dbName string) *DocumentDB {
	d.openDOcDBMu.Lock()
	defer d.openDOcDBMu.Unlock()
	db, found := d.openDocDBs[dbName]
	if !found {
		return nil
	}
	return db
}

func (d *Document) addToOpenedDb(dbName string, docDB *DocumentDB) {
	d.openDOcDBMu.Lock()
	defer d.openDOcDBMu.Unlock()
	d.openDocDBs[dbName] = docDB
}

func (d *Document) resolveExpression(expr string) (string, string, string, error) {
	var operator string
	if strings.Contains(expr, "=>") {
		operator = "=>"
	} else if strings.Contains(expr, "<=") {
		operator = "<="
	} else if strings.Contains(expr, ">") {
		operator = ">"
	} else if strings.Contains(expr, "=") {
		operator = "="
	} else {
		return "", "", "", ErrInvalidOperator
	}

	f := strings.Split(expr, operator)
	fieldName := f[0]
	fieldValue := f[1]

	return fieldName, operator, fieldValue, nil
}

func (d *Document) CreateDocBatch(name string) (*DocBatch, error) {
	if d.fd.IsReadOnlyFeed() {
		return nil, ErrReadOnlyIndex
	}

	d.openDOcDBMu.Lock()
	defer d.openDOcDBMu.Unlock()
	if db, ok := d.openDocDBs[name]; ok {
		var docBatch DocBatch
		docBatch.db = db
		docBatch.batches = make(map[string]*Batch)

		for fieldName, idx := range db.simpleIndexes {
			batch, err := NewBatch(idx)
			if err != nil {
				return nil, err
			}
			docBatch.batches[fieldName] = batch
		}
		for fieldName, idx := range db.mapIndexes {
			batch, err := NewBatch(idx)
			if err != nil {
				return nil, err
			}
			docBatch.batches[fieldName] = batch
		}
		for fieldName, idx := range db.listIndexes {
			batch, err := NewBatch(idx)
			if err != nil {
				return nil, err
			}
			docBatch.batches[fieldName] = batch
		}

		return &docBatch, nil
	}
	return nil, ErrDocumentDBNotOpened
}

func (d *Document) DocBatchPut(docBatch *DocBatch, doc []byte) error {
	if d.fd.IsReadOnlyFeed() {
		return ErrReadOnlyIndex
	}

	d.openDOcDBMu.Lock()
	defer d.openDOcDBMu.Unlock()

	var t interface{}
	err := json.Unmarshal(doc, &t)
	if err != nil {
		return err
	}
	docMap := t.(map[string]interface{})

	// check if docMap has all the fields in the simpleIndex
	for field := range docBatch.db.simpleIndexes {
		if _, found := docMap[field]; !found {
			return ErrDocumentDBIndexFieldNotPresent
		}
	}

	// check if the id is already present
	// and remove it if it is present
	var valStr string
	idValue := docMap[DefaultIndexFieldName]
	switch v := idValue.(type) {
	case float64:
		val1 := int64(v)
		valStr = strconv.FormatInt(val1, 10)
	case string:
		valStr = v
	default:
		return ErrInvalidIndexType
	}

	if valStr == "" {
		return ErrInvalidDocumentId
	} else {
		idBatchIndex := docBatch.batches[DefaultIndexFieldName]
		refs, err := idBatchIndex.Get(valStr)
		if err == nil {
			// found a doc with the same id, so remove it and all the indexes
			if len(refs) > 0 {
				data, _, err := d.client.DownloadBlob(refs[0])
				if err != nil {
					return err
				}

				var t interface{}
				err = json.Unmarshal(data, &t)
				if err != nil {
					return err
				}
				oldDocMap := t.(map[string]interface{})

				for field, batchIndex := range docBatch.batches {
					v1 := oldDocMap[field] // it is already checked to be present
					switch batchIndex.idx.indexType {
					case StringIndex:
						_, err := batchIndex.Del(v1.(string))
						if err != nil {
							return err
						}
					case MapIndex:
						valMap := v1.(map[string]interface{})
						for keyField, valueField := range valMap {
							vf := valueField.(string)
							mapField := keyField + vf
							_, err := batchIndex.Del(mapField)
							if err != nil {
								return err
							}
						}
					case ListIndex:
						valList := v1.([]string)
						for _, listVal := range valList {
							_, err := batchIndex.Del(listVal)
							if err != nil {
								return err
							}
						}
					case NumberIndex:
						val := v1.(float64)
						val1 := int64(val)
						valStr := strconv.FormatInt(val1, 10)
						_, err := batchIndex.Del(valStr)
						if err != nil {
							return err
						}
					case BytesIndex:
						return ErrIndexNotSupported
					default:
						return ErrInvalidIndexType
					}
				}

				err = d.client.DeleteBlob(refs[0])
				if err != nil {
					return err
				}

			}
		}
	}

	// upload the document
	ref, err := d.client.UploadBlob(doc, true, true)
	if err != nil {
		return err
	}

	// update the indexes
	for field, batchIndex := range docBatch.batches {
		if v, found := docMap[field]; found { // it is already checked to be present
			switch batchIndex.idx.indexType {
			case StringIndex:
				var valStr1 string
				switch v := v.(type) {
				case float64:
					val1 := int64(v)
					valStr1 = strconv.FormatInt(val1, 10)
				case string:
					valStr1 = v
				default:
					return ErrInvalidIndexType
				}

				apnd := true
				if field == DefaultIndexFieldName {
					apnd = false
				}
				err := batchIndex.Put(valStr1, ref, apnd)
				if err != nil {
					return err
				}
			case MapIndex:
				valMap := v.(map[string]interface{})
				for keyField, valueField := range valMap {
					vf := valueField.(string)
					mapField := keyField + vf
					err := batchIndex.Put(mapField, ref, true)
					if err != nil {
						return err
					}
				}
			case ListIndex:
				valList := v.([]string)
				for _, listVal := range valList {
					listField := listVal
					err := batchIndex.Put(listField, ref, true)
					if err != nil {
						return err
					}
				}
			case NumberIndex:
				var valStr string
				switch v1 := v.(type) {
				case string:
					valStr = v1
				case float64:
					val := v1
					val1 := int64(val)
					valStr = strconv.FormatInt(val1, 10)
				default:
					return ErrIndexNotSupported
				}
				err := batchIndex.Put(valStr, ref, true)
				if err != nil {
					return err
				}
			case BytesIndex:
				return ErrIndexNotSupported
			default:
				return ErrInvalidIndexType
			}
		}
	}

	return nil
}

func (d *Document) DocBatchWrite(docBatch *DocBatch) error {
	if d.fd.IsReadOnlyFeed() {
		return ErrReadOnlyIndex
	}
	for _, batch := range docBatch.batches {
		err := batch.Write()
		if err != nil {
			return err
		}
	}
	return nil
}
