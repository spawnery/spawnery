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
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/spawnery/spawnery/internal/prune"
)

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

func openDir(root string, parts []string, create bool) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, pathErr("open", root, nil, err)
	}
	for i, p := range parts {
		next, err := unix.Openat(fd, p, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(fd, p, 0o755); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, p, dirFlags, 0)
			}
		}
		_ = unix.Close(fd)
		if err != nil {
			return -1, pathErr("open", root, parts[:i+1], err)
		}
		fd = next
	}
	return fd, nil
}

func parentOf(root, rel string, create bool) (int, string, error) {
	parts, err := relParts(rel)
	if err != nil {
		return -1, "", err
	}
	fd, err := openDir(root, parts[:len(parts)-1], create)
	return fd, parts[len(parts)-1], err
}

// openRegular opens root/rel for reading only if it is a regular file. The
// O_PATH descriptor is checked before the file is opened through it, so a
// FIFO or device swapped in is never opened. O_NONBLOCK makes a pod's write
// lease fail the open instead of holding it for fs.lease-break-time.
func openRegular(root, rel string) (*os.File, error) {
	dir, name, err := parentOf(root, rel, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dir) }()
	path := filepath.Join(root, filepath.FromSlash(rel))
	pfd, err := unix.Openat(dir, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = unix.Close(pfd) }()
	var st unix.Stat_t
	if err := unix.Fstat(pfd, &st); err != nil {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("worldsync: %s is not a regular file", path)
	}
	fd, err := unix.Open("/proc/self/fd/"+strconv.Itoa(pfd), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func lstatAt(root, rel string) error {
	dir, name, err := parentOf(root, rel, false)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dir) }()
	var st unix.Stat_t
	return unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW)
}

// place writes r to root/rel through a temporary file beside it and renames
// that over rel, which replaces a symlink at rel instead of writing through it.
func place(root, rel string, r io.Reader, mode uint32, mtime int64) error {
	dir, name, err := parentOf(root, rel, true)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dir) }()
	tmp := name + ".worldsync-part"
	path := filepath.Join(root, filepath.FromSlash(rel)) + ".worldsync-part"
	if err := unix.Unlinkat(dir, tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return &fs.PathError{Op: "remove", Path: path, Err: err}
	}
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return &fs.PathError{Op: "create", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	placed := false
	defer func() {
		if !placed {
			_ = unix.Unlinkat(dir, tmp, 0)
		}
	}()
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if mode != 0 {
		if err := f.Chmod(os.FileMode(mode)); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ts := unix.NsecToTimespec(mtime)
	if err := unix.UtimesNanoAt(dir, tmp, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "chtimes", Path: path, Err: err}
	}
	if err := unix.Renameat(dir, tmp, dir, name); err != nil {
		return &fs.PathError{Op: "rename", Path: path, Err: err}
	}
	placed = true
	return nil
}

// walkKept visits every entry below root other than a directory that keep
// holds or leads toward, and descends into such directories only. dir is
// the open directory that holds name.
func walkKept(root string, keep prune.Keep, visit func(dir int, name, rel string, st *unix.Stat_t) error) error {
	fd, err := openDir(root, nil, false)
	if err != nil {
		return err
	}
	return walkKeptIn(root, fd, nil, keep, visit)
}

// walkKeptIn takes over fd.
func walkKeptIn(root string, fd int, parts []string, keep prune.Keep, visit func(dir int, name, rel string, st *unix.Stat_t) error) error {
	d := os.NewFile(uintptr(fd), root)
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if len(parts) == 0 && name == prune.ControlDir {
			continue
		}
		child := append(append([]string(nil), parts...), name)
		rel := strings.Join(child, "/")
		if len(rel) > maxRel {
			return fmt.Errorf("worldsync: %s holds a path longer than the %d bytes a world may hold", root, maxRel)
		}
		if !keep.Holds(rel) && !keep.Toward(rel) {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return pathErr("lstat", root, child, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := visit(fd, name, rel, &st); err != nil {
				return err
			}
			continue
		}
		sub, err := unix.Openat(fd, name, dirFlags, 0)
		if err != nil {
			return pathErr("open", root, child, err)
		}
		if err := walkKeptIn(root, sub, child, keep, visit); err != nil {
			return err
		}
	}
	return nil
}
