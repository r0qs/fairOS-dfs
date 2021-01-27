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

package pod

import (
	"io/ioutil"
	"strings"
	"testing"
	"time"

	"github.com/fairdatasociety/fairOS-dfs/pkg/account"
	"github.com/fairdatasociety/fairOS-dfs/pkg/blockstore/bee/mock"
	"github.com/fairdatasociety/fairOS-dfs/pkg/common"
	"github.com/fairdatasociety/fairOS-dfs/pkg/feed"
	"github.com/fairdatasociety/fairOS-dfs/pkg/logging"
)

func TestPod_MakeDir(t *testing.T) {
	mockClient := mock.NewMockBeeClient()
	logger := logging.New(ioutil.Discard, 0)
	acc := account.New(logger)
	_, _, err := acc.CreateUserAccount("password", "")
	if err != nil {
		t.Fatal(err)
	}
	fd := feed.New(acc.GetUserAccountInfo(), mockClient, logger)
	pod1 := NewPod(mockClient, fd, acc, logger)

	podName1 := "test1"
	podName2 := "test2"
	podName3 := "test3"
	podName4 := "test4"
	podName5 := "test5"
	firstDir := "dir1"
	secondDir := "dir2"
	thirdDir := "dir3/dir4"
	fourthDir := "/dir5"
	t.Run("mkdir-on-root-of-pod", func(t *testing.T) {
		info, err := pod1.CreatePod(podName1, "password", "")
		if err != nil {
			t.Fatalf("error creating pod %s", podName1)
		}

		err = pod1.MakeDir(podName1, firstDir)
		if err != nil {
			t.Fatalf("error creating directory %s", firstDir)
		}

		dirPath := common.PathSeperator + podName1 + common.PathSeperator + firstDir
		dirInode := info.GetDirectory().GetDirFromDirectoryMap(dirPath)
		if dirInode == nil {
			t.Fatalf("directory not created")
		}
		if dirInode.Meta.Path != common.PathSeperator+podName1 {
			t.Fatalf("invalid path in meta")
		}
		if dirInode.Meta.Name != firstDir {
			t.Fatalf("invalid name in meta")
		}
		if dirInode.GetDirInodePathAndName() != dirPath {
			t.Fatalf("invalid path or name")
		}

		// cleanup pod
		err = pod1.DeletePod(podName1)
		if err != nil {
			t.Fatalf("could not delete pod")
		}
	})

	t.Run("mkdir-second-dir-from-first-dir", func(t *testing.T) {
		info, err := pod1.CreatePod(podName2, "password", "")
		if err != nil {
			t.Fatalf("error creating pod %s", podName2)
		}

		err = pod1.MakeDir(podName2, firstDir)
		if err != nil {
			t.Fatalf("error creating directory %s", firstDir)
		}

		_, err = pod1.ChangeDir(podName2, firstDir)
		if err != nil {
			t.Fatalf("error changing directory")
		}

		err = pod1.MakeDir(podName2, secondDir)
		if err != nil {
			t.Fatalf("error creating directory %s", secondDir)
		}

		dirPath := common.PathSeperator + podName2 + common.PathSeperator + firstDir + common.PathSeperator + secondDir
		dirInode := info.GetDirectory().GetDirFromDirectoryMap(dirPath)
		if dirInode == nil {
			t.Fatalf("directory not created")
		}
		if dirInode.Meta.Path != common.PathSeperator+podName2+common.PathSeperator+firstDir {
			t.Fatalf("invalid path in meta")
		}
		if dirInode.Meta.Name != secondDir {
			t.Fatalf("invalid name in meta")
		}
		if dirInode.GetDirInodePathAndName() != dirPath {
			t.Fatalf("invalid path or name")
		}

		// cleanup directory and pod
		err = pod1.DeletePod(podName2)
		if err != nil {
			t.Fatalf("could not delete pod")
		}
	})

	t.Run("mkdir-second-dir-from-pod", func(t *testing.T) {
		info, err := pod1.CreatePod(podName3, "password", "")
		if err != nil {
			t.Fatalf("error creating pod %s", podName3)
		}

		err = pod1.MakeDir(podName3, firstDir)
		if err != nil {
			t.Fatalf("error creating directory %s", err)
		}
		time.Sleep(1 * time.Second)
		err = pod1.MakeDir(podName3, firstDir+common.PathSeperator+secondDir)
		if err != nil {
			t.Fatalf("error creating directory %s", err)
		}

		dirPath := common.PathSeperator + podName3 + common.PathSeperator + firstDir + common.PathSeperator + secondDir
		dirInode := info.GetDirectory().GetDirFromDirectoryMap(dirPath)
		if dirInode == nil {
			t.Fatalf("directory not created")
		}
		if dirInode.Meta.Path != common.PathSeperator+podName3+common.PathSeperator+"dir1" {
			t.Fatalf("invalid path in meta")
		}
		if dirInode.Meta.Name != "dir2" {
			t.Fatalf("invalid name in meta")
		}
		if dirInode.GetDirInodePathAndName() != dirPath {
			t.Fatalf("invalid path or name")
		}

		// cleanup directory and pod
		err = pod1.DeletePod(podName3)
		if err != nil {
			t.Fatalf("could not delete pod")
		}
	})

	t.Run("mkdir-multiple-dirs-from-pod", func(t *testing.T) {
		info, err := pod1.CreatePod(podName4, "password", "")
		if err != nil {
			t.Fatalf("error creating pod %s", podName4)
		}

		err = pod1.MakeDir(podName4, thirdDir)
		if err != nil {
			t.Fatalf("error creating directory %s", thirdDir)
		}

		// check /test/dir3
		dirPath := common.PathSeperator + podName4 + common.PathSeperator + "dir3"
		dirInode := info.GetDirectory().GetDirFromDirectoryMap(dirPath)
		if dirInode == nil {
			t.Fatalf("directory not created")
		}
		if dirInode.Meta.Path != common.PathSeperator+podName4 {
			t.Fatalf("invalid path in meta")
		}
		if dirInode.Meta.Name != "dir3" {
			t.Fatalf("invalid name in meta")
		}
		if dirInode.GetDirInodePathAndName() != dirPath {
			t.Fatalf("invalid path or name")
		}

		// check /test/dir3/dir4
		dirPath = common.PathSeperator + podName4 + common.PathSeperator + thirdDir
		dirInode = info.GetDirectory().GetDirFromDirectoryMap(dirPath)
		if dirInode == nil {
			t.Fatalf("directory not created")
		}
		if dirInode.Meta.Path != common.PathSeperator+podName4+common.PathSeperator+"dir3" {
			t.Fatalf("invalid path in meta")
		}
		if dirInode.Meta.Name != "dir4" {
			t.Fatalf("invalid name in meta")
		}
		if dirInode.GetDirInodePathAndName() != dirPath {
			t.Fatalf("invalid path or name")
		}

		// cleanup directory and pod
		err = pod1.DeletePod(podName4)
		if err != nil {
			t.Fatalf("could not delete pod")
		}
	})

	t.Run("mkdir-with-slash-on-pod", func(t *testing.T) {
		info, err := pod1.CreatePod(podName5, "password", "")
		if err != nil {
			t.Fatalf("error creating pod %s", podName5)
		}

		err = pod1.MakeDir(podName5, fourthDir)
		if err != nil {
			t.Fatalf("error creating directory %s", fourthDir)
		}

		dirPath := common.PathSeperator + podName5 + fourthDir
		dirInode := info.GetDirectory().GetDirFromDirectoryMap(dirPath)
		if dirInode == nil {
			t.Fatalf("directory not created")
		}
		if dirInode.Meta.Path != common.PathSeperator+podName5 {
			t.Fatalf("invalid path in meta")
		}
		if dirInode.Meta.Name != strings.TrimPrefix(fourthDir, "/") {
			t.Fatalf("invalid name in meta")
		}
		if dirInode.GetDirInodePathAndName() != dirPath {
			t.Fatalf("invalid path or name")
		}

		// cleanup directory and pod
		err = pod1.DeletePod(podName5)
		if err != nil {
			t.Fatalf("could not delete pod")
		}
	})
}
