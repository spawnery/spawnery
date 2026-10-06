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

// openDir gives a directory it creates to gid; a negative gid leaves it root's.
func openDir(root string, parts []string, create bool, gid int) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, pathErr("open", root, nil, err)
	}
	for i, p := range parts {
		next, err := unix.Openat(fd, p, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) && create {
			made := unix.Mkdirat(fd, p, 0o755)
			if made == nil || errors.Is(made, unix.EEXIST) {
				next, err = unix.Openat(fd, p, dirFlags, 0)
			} else {
				err = made
			}
			if err == nil && made == nil && gid >= 0 {
				if err = regroup(next, gid, groupDir); err != nil {
					_ = unix.Close(next)
				}
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

func parentOf(root, rel string, create bool, gid int) (int, string, error) {
	parts, err := relParts(rel)
	if err != nil {
		return -1, "", err
	}
	fd, err := openDir(root, parts[:len(parts)-1], create, gid)
	return fd, parts[len(parts)-1], err
}

// regroup gives fd, an open directory or an O_PATH descriptor of a regular
// file, to gid and adds bits to its mode. A chown may clear setgid, so the
// mode is set again after one.
func regroup(fd, gid int, bits uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	moved := int(st.Gid) != gid
	if moved {
		if err := unix.Fchownat(fd, "", -1, gid, unix.AT_EMPTY_PATH); err != nil {
			return err
		}
	}
	if !moved && st.Mode&bits == bits {
		return nil
	}
	return unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), st.Mode&0o7777|bits)
}

// openRegular opens root/rel for reading only if it is a regular file. The
// O_PATH descriptor is checked before the file is opened through it, so a
// FIFO or device swapped in is never opened. O_NONBLOCK makes a pod's write
// lease fail the open instead of holding it for fs.lease-break-time.
func openRegular(root, rel string) (*os.File, error) {
	dir, name, err := parentOf(root, rel, false, -1)
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
	dir, name, err := parentOf(root, rel, false, -1)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dir) }()
	var st unix.Stat_t
	return unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW)
}

// place writes r to root/rel through a temporary file beside it and renames
// that over rel, which replaces a symlink at rel instead of writing through it.
// The file and the directories it creates go to gid unless gid is negative.
func place(root, rel string, r io.Reader, mode uint32, mtime int64, gid int) error {
	dir, name, err := parentOf(root, rel, true, gid)
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
	if gid >= 0 {
		if err := unix.Fchown(fd, -1, gid); err != nil {
			_ = f.Close()
			return &fs.PathError{Op: "chown", Path: path, Err: err}
		}
		if mode == 0 {
			mode = 0o644
		}
		mode |= groupFile
	}
	if mode != 0 {
		if err := unix.Fchmod(fd, mode); err != nil {
			_ = f.Close()
			return &fs.PathError{Op: "chmod", Path: path, Err: err}
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
	fd, err := openDir(root, nil, false, -1)
	if err != nil {
		return err
	}
	return walkKeptIn(root, fd, nil, keep, visit, nil)
}

// walkKeptIn takes over fd. enter, if set, sees each directory it descends
// into, open.
func walkKeptIn(root string, fd int, parts []string, keep prune.Keep, visit func(dir int, name, rel string, st *unix.Stat_t) error, enter func(fd int, rel string) error) error {
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
		if enter != nil {
			if err := enter(sub, rel); err != nil {
				_ = unix.Close(sub)
				return err
			}
		}
		if err := walkKeptIn(root, sub, child, keep, visit, enter); err != nil {
			return err
		}
	}
	return nil
}

// regroupKept gives gid root itself, the control directory with its files,
// and every directory and regular file keep holds or leads toward. Symlinks,
// FIFOs and devices a pod planted are left alone.
func regroupKept(root string, keep prune.Keep, gid int) error {
	fd, err := openDir(root, nil, false, -1)
	if err != nil {
		return err
	}
	if err := regroup(fd, gid, groupDir); err != nil {
		_ = unix.Close(fd)
		return pathErr("chown", root, nil, err)
	}
	enter := func(sub int, rel string) error {
		if err := regroup(sub, gid, groupDir); err != nil {
			return pathErr("chown", root, []string{rel}, err)
		}
		return nil
	}
	file := func(dir int, name, rel string, st *unix.Stat_t) error {
		if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Gid) == gid && st.Mode&groupFile == groupFile {
			return nil
		}
		pfd, err := unix.Openat(dir, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return pathErr("open", root, []string{rel}, err)
		}
		defer func() { _ = unix.Close(pfd) }()
		var now unix.Stat_t
		if err := unix.Fstat(pfd, &now); err != nil {
			return pathErr("stat", root, []string{rel}, err)
		}
		if now.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil
		}
		if err := regroup(pfd, gid, groupFile); err != nil {
			return pathErr("chown", root, []string{rel}, err)
		}
		return nil
	}
	if err := walkKeptIn(root, fd, nil, keep, file, enter); err != nil {
		return err
	}
	ctl, err := openDir(root, []string{prune.ControlDir}, false, -1)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := enter(ctl, prune.ControlDir); err != nil {
		_ = unix.Close(ctl)
		return err
	}
	all, err := prune.ParseKeep([]string{"*"})
	if err != nil {
		_ = unix.Close(ctl)
		return err
	}
	return walkKeptIn(root, ctl, []string{prune.ControlDir}, all, file, enter)
}
