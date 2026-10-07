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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// chain makes root/a/a/... depth levels deep; the path is too long to name
// from root, so it is built with *at calls.
func chain(t *testing.T, root string, depth int) {
	t.Helper()
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range depth {
		if err := unix.Mkdirat(fd, "a", 0o755); err != nil {
			t.Fatal(err)
		}
		next, err := unix.Openat(fd, "a", unix.O_RDONLY|unix.O_DIRECTORY, 0)
		_ = unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		fd = next
	}
	_ = unix.Close(fd)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(root, "a")) })
}

func TestAScanRefusesATreeDeeperThanAPathCanBe(t *testing.T) {
	data := t.TempDir()
	chain(t, data, 2100)
	start := time.Now()
	var err error
	withTimeout(t, "a scan of a deep chain", func() { _, err = Scan(data, keepOf(t, "a")) }, func() {})
	if err == nil {
		t.Error("a scan of a chain 2100 levels deep succeeded")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the scan took %v", d)
	}
}

func TestAWriteLeaseDoesNotHoldUpARead(t *testing.T) {
	data := t.TempDir()
	path := filepath.Join(data, "worlds/world/level.dat")
	writeFile(t, path, 10, time.Unix(1, 0))
	held, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	if _, err := unix.FcntlInt(held.Fd(), unix.F_SETLEASE, unix.F_WRLCK); err != nil {
		t.Skipf("no write lease on this filesystem: %v", err)
	}

	start := time.Now()
	var opened *os.File
	withTimeout(t, "a read of a leased file", func() {
		opened, _ = openRegular(data, "worlds/world/level.dat")
	}, func() { _, _ = unix.FcntlInt(held.Fd(), unix.F_SETLEASE, unix.F_UNLCK) })
	if opened != nil {
		_ = opened.Close()
		t.Error("a file under a write lease was opened")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the open took %v", d)
	}
}

func TestAPlacedFileNeverCarriesSetuidOrSetgid(t *testing.T) {
	for _, gid := range []int{-1, os.Getegid()} {
		data := t.TempDir()
		if err := place(data, "plugins/x/run.sh", strings.NewReader("x"), 0o6755, 1, gid); err != nil {
			t.Fatal(err)
		}
		st := statOf(t, filepath.Join(data, "plugins/x/run.sh"))
		if st.Mode&0o7000 != 0 {
			t.Errorf("gid %d: placed with mode %o; a manifest must not set setuid, setgid or sticky", gid, st.Mode&0o7777)
		}
	}
}

func TestNewNodeClosesItsDirectoriesToOtherUsers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	for _, d := range []string{root, filepath.Join(root, "worlds"), filepath.Join(root, "orphans")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewNode(Config{Root: root, NodeID: "a", Store: NewMemStore(time.Now), Mounter: &fakeMounter{binds: map[string]string{}}}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{root, filepath.Join(root, "worlds"), filepath.Join(root, "orphans")} {
		if m := statOf(t, d).Mode & 0o777; m != 0o700 {
			t.Errorf("%s: mode %o, want 700", d, m)
		}
	}
}
