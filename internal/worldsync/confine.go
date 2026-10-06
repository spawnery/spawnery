/*
Copyright paul_wtf.

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

package worldsync

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A pod may swap any entry below its data directory for a symlink, a FIFO
// or a device node at any moment. Paths below it are never resolved by
// name: confine_linux.go walks them one component at a time from an open
// directory with O_NOFOLLOW and acts on the last component with *at calls.
// Other systems get no such walk and refuse.

// maxRel is PATH_MAX: a deeper file could not be copied by name anyway,
// and a pod builds a chain of directories far deeper in milliseconds.
const maxRel = 4096

// The bits the pod's group gets on what the node agent makes for it.
// Directories are setgid, so that what the pod creates in them stays in
// the group.
const groupDir, groupFile = 0o2070, 0o060

func relParts(rel string) ([]string, error) {
	if len(rel) > maxRel {
		return nil, fmt.Errorf("worldsync: a path of %d bytes is longer than the %d a world may hold", len(rel), maxRel)
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("worldsync: path %q leaves the world", rel)
		}
	}
	return parts, nil
}

func pathErr(op, root string, parts []string, err error) error {
	return &fs.PathError{Op: op, Path: filepath.Join(root, filepath.Join(parts...)), Err: err}
}

func readSmall(root, rel string, limit int64) ([]byte, error) {
	f, err := openRegular(root, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}

// removeAllAt removes a symlink at root/name itself, not what it points to.
func removeAllAt(root, name string) error {
	r, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return r.RemoveAll(name)
}
