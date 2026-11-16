//go:build !linux

/*
Copyright The Treebind Authors.

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

package scan

import "io/fs"

// identityOf has nothing to return where the agent does not run: it runs in a Linux container, and this
// is here so that the package compiles on the laptops that develop it. The change time and the inode are
// zero, and a scan there sees a file by its mode, size and modification time.
func identityOf(fs.FileInfo) (changeTime int64, inode uint64) { return 0, 0 }
