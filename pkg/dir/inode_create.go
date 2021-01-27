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

package dir

import (
	"encoding/json"
	"time"

	"github.com/fairdatasociety/fairOS-dfs/pkg/common"
	m "github.com/fairdatasociety/fairOS-dfs/pkg/meta"
)

func (d *Directory) CreateDirINode(podName, dirName string, parent *DirInode) (*DirInode, []byte, error) {
	// create the meta data
	parentPath := getPath(podName, parent)
	now := time.Now().Unix()
	meta := m.DirectoryMetaData{
		Version:          m.DirMetaVersion,
		Path:             parentPath,
		Name:             dirName,
		CreationTime:     now,
		ModificationTime: now,
		AccessTime:       now,
	}
	dirInode := &DirInode{
		Meta: &meta,
	}
	data, err := json.Marshal(dirInode)
	if err != nil {
		return nil, nil, err
	}

	// create a feed for the directory and add data to it
	totalPath := parentPath + common.PathSeperator + dirName
	topic := common.HashString(totalPath)
	_, err = d.fd.CreateFeed(topic, d.acc.GetAddress(), data)
	if err != nil {
		return nil, nil, err
	}

	d.AddToDirectoryMap(totalPath, dirInode)
	return dirInode, topic, nil
}

func (d *Directory) IsDirINodePresent(podName, dirName string, parent *DirInode) bool {
	parentPath := getPath(podName, parent)
	totalPath := parentPath + common.PathSeperator + dirName
	topic := common.HashString(totalPath)
	_, _, err := d.fd.GetFeedData(topic, d.getAccount().GetAddress())
	return err == nil
}

func getPath(podName string, parent *DirInode) string {
	var path string
	if parent.Meta.Path == common.PathSeperator {
		path = parent.Meta.Path + parent.Meta.Name
	} else {
		path = parent.Meta.Path + common.PathSeperator + parent.Meta.Name
	}
	return path
}

func (d *Directory) CreatePodINode(podName string) (*DirInode, []byte, error) {
	// create the metadata
	now := time.Now().Unix()
	meta := m.DirectoryMetaData{
		Version:          m.DirMetaVersion,
		Path:             "/",
		Name:             podName,
		CreationTime:     now,
		ModificationTime: now,
		AccessTime:       now,
	}
	dirInode := &DirInode{
		Meta: &meta,
	}
	data, err := json.Marshal(dirInode)
	if err != nil {
		return nil, nil, err
	}

	// create a feed and store the metadata of the pod
	totalPath := common.PathSeperator + podName
	topic := common.HashString(totalPath)
	_, err = d.fd.CreateFeed(topic, d.acc.GetAddress(), data)
	if err != nil {
		return nil, nil, err
	}

	d.AddToDirectoryMap(totalPath, dirInode)
	return dirInode, topic, nil
}

func (d *Directory) DeletePodInode(podName string) error {
	totalPath := common.PathSeperator + podName
	topic := common.HashString(totalPath)
	return d.fd.DeleteFeed(topic, d.acc.GetAddress())
}

func (d *Directory) DeleteDirectoryInode(dirPath string) error {
	topic := common.HashString(dirPath)
	return d.fd.DeleteFeed(topic, d.acc.GetAddress())
}
