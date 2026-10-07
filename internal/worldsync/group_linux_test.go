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
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr/funcr"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/spawnery/spawnery/internal/prune"
)

// podGroup is a group the test may chown to other than its own, so that a
// chown shows; without one, the chown checks see only the mode bits.
func podGroup(t *testing.T) int {
	t.Helper()
	if os.Geteuid() == 0 {
		return 4242
	}
	gids, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gids {
		if g != os.Getegid() {
			return g
		}
	}
	t.Logf("no supplementary group: only the mode bits tell a chown apart")
	return os.Getegid()
}

func statOf(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func assertShared(t *testing.T, path string, gid int) {
	t.Helper()
	st := statOf(t, path)
	want := uint32(groupFile)
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		want = groupDir
	}
	if int(st.Gid) != gid || st.Mode&want != want {
		t.Errorf("%s: gid %d mode %o; want gid %d with the bits %o", path, st.Gid, st.Mode&0o7777, gid, want)
	}
}

func (h *harness) publishAs(id, world, pod string, gid int) (string, error) {
	target := filepath.Join(h.t.TempDir(), "mount")
	return target, h.node(id).Publish(context.Background(), PublishRequest{World: world, Keep: []string{"worlds/world"}, Target: target, Pod: pod, Group: &gid})
}

func TestANewWorldIsThePodsGroupAtOnce(t *testing.T) {
	h := newHarness(t)
	gid := podGroup(t)
	if _, err := h.publishAs("a", w, "p1", gid); err != nil {
		t.Fatal(err)
	}
	data := h.node("a").dataDir(w)
	for _, rel := range []string{"", prune.ControlDir, ReadyFile} {
		assertShared(t, filepath.Join(data, rel), gid)
	}
}

func TestADownloadedWorldIsThePodsGroup(t *testing.T) {
	h := newHarness(t)
	gid := podGroup(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	writeFile(t, filepath.Join(target, "worlds/world/region/r.0.0.mca"), PackBelow+1, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	target, err := h.publishAs("b", w, "p2", gid)
	if err != nil {
		t.Fatal(err)
	}
	h.waitFile(target, ReadyFile)
	data := h.node("b").dataDir(w)
	for _, rel := range []string{"", "worlds", "worlds/world", "worlds/world/region", "worlds/world/level.dat",
		"worlds/world/region/r.0.0.mca", prune.ControlDir, ReadyFile} {
		assertShared(t, filepath.Join(data, rel), gid)
	}

	writeFile(t, filepath.Join(target, "worlds/world/region/r.0.1.mca"), 5, h.now)
	if st := statOf(t, filepath.Join(data, "worlds/world/region/r.0.1.mca")); int(st.Gid) != gid {
		t.Errorf("a file the pod creates has gid %d, want %d from the setgid directory", st.Gid, gid)
	}
	h.request(target, "1")
	h.waitFile(target, DoneFile)
	assertShared(t, filepath.Join(data, DoneFile), gid)
}

func TestAnEarlierCacheIsBroughtToThePodsGroup(t *testing.T) {
	h := newHarness(t)
	gid := podGroup(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	writeFile(t, filepath.Join(target, "worlds/world/region/r.0.0.mca"), PackBelow+1, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	data := h.node("a").dataDir(w)
	away, file := outside(t, "victim", []byte("untouched"))
	symlink(t, file, filepath.Join(data, "worlds/world/session.lock"))
	symlink(t, away, filepath.Join(data, "worlds/world/data"))
	awayBefore, fileBefore := statOf(t, away), statOf(t, file)

	downloads := downloadsCounted(h.node("a"))
	if _, err := h.publishAs("a", w, "p2", gid); err != nil {
		t.Fatal(err)
	}
	if downloadsCounted(h.node("a")) != downloads {
		t.Fatal("the cache was downloaded again instead of brought to the group")
	}
	for _, rel := range []string{"", "worlds", "worlds/world", "worlds/world/region", "worlds/world/level.dat",
		"worlds/world/region/r.0.0.mca", prune.ControlDir, ReadyFile} {
		assertShared(t, filepath.Join(data, rel), gid)
	}
	for path, before := range map[string]unix.Stat_t{away: awayBefore, file: fileBefore} {
		if after := statOf(t, path); after.Gid != before.Gid || after.Mode != before.Mode {
			t.Errorf("%s behind a planted symlink: gid %d mode %o, was gid %d mode %o", path, after.Gid, after.Mode&0o7777, before.Gid, before.Mode&0o7777)
		}
	}
}

func TestARepublishOfTheSameTargetBringsTheCacheToTheGroup(t *testing.T) {
	h := newHarness(t)
	gid := podGroup(t)
	target := filepath.Join(t.TempDir(), "mount")
	req := PublishRequest{World: w, Keep: []string{"worlds/world"}, Target: target, Pod: "p1"}
	if err := h.node("a").Publish(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	req.Group = &gid
	if err := h.node("a").Publish(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	data := h.node("a").dataDir(w)
	for _, rel := range []string{"", "worlds", "worlds/world", "worlds/world/level.dat", prune.ControlDir, ReadyFile} {
		assertShared(t, filepath.Join(data, rel), gid)
	}
}

func TestWithoutAGroupTheWorldStaysRootsAndThePublishSaysSo(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var said []string
	n := h.node("a")
	n.cfg.Log = funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		said = append(said, args)
	}, funcr.Options{})

	if _, err := h.publish("a", w, "p1"); err != nil {
		t.Fatal(err)
	}
	data := n.dataDir(w)
	for _, rel := range []string{"", prune.ControlDir, ReadyFile} {
		if st := statOf(t, filepath.Join(data, rel)); st.Mode&0o2020 != 0 {
			t.Errorf("%s: mode %o without a group; want no group write and no setgid", rel, st.Mode&0o7777)
		}
	}
	if _, err := h.publishAs("a", "ns/g/other", "p2", podGroup(t)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	warned := 0
	for _, l := range said {
		if strings.Contains(l, "fsGroup") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("%d warnings about the missing fsGroup in %q, want one", warned, said)
	}
}

func TestThePodsFSGroupReachesTheWorldOverCSI(t *testing.T) {
	s, h := csiFor(t)
	gid := podGroup(t)
	req := publishReq(t, "ns", w)
	req.VolumeCapability.GetMount().VolumeMountGroup = strconv.Itoa(gid)
	if _, err := s.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if g := h.node("a").lookup(w).Group; g == nil || *g != gid {
		t.Fatalf("the world state holds group %v, want %d", g, gid)
	}
	assertShared(t, h.node("a").dataDir(w), gid)
}

func TestAMountGroupThatIsNoGidIsInvalidArgument(t *testing.T) {
	for _, g := range []string{"abc", "-1", "+5", " 5", "2147483648", "4294967295"} {
		t.Run(g, func(t *testing.T) {
			s, _ := csiFor(t)
			req := publishReq(t, "ns", w)
			req.VolumeCapability = &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{VolumeMountGroup: g}},
				AccessMode: req.VolumeCapability.AccessMode,
			}
			if _, err := s.NodePublishVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v (%v), want InvalidArgument", status.Code(err), err)
			}
		})
	}
}

func TestARepublishSkipsAnEntryThePodRemovesDuringTheRegroup(t *testing.T) {
	h := newHarness(t)
	gid := podGroup(t)
	target := filepath.Join(t.TempDir(), "mount")
	req := PublishRequest{World: w, Keep: []string{"worlds/world"}, Target: target, Pod: "p1"}
	if err := h.node("a").Publish(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.dat", "b.dat"} {
		writeFile(t, filepath.Join(target, "worlds/world", f), 5, h.now)
	}
	if err := os.MkdirAll(filepath.Join(target, "worlds/world/gone"), 0o755); err != nil {
		t.Fatal(err)
	}
	removed := false
	walkHook = func(rel string) {
		if removed || !strings.HasPrefix(rel, "worlds/world/") {
			return
		}
		removed = true
		for _, f := range []string{"a.dat", "b.dat", "gone"} {
			if p := "worlds/world/" + f; p != rel {
				_ = os.RemoveAll(filepath.Join(target, p))
			}
		}
	}
	t.Cleanup(func() { walkHook = nil })

	req.Group = &gid
	if err := h.node("a").Publish(context.Background(), req); err != nil {
		t.Fatalf("a file renamed away during the regroup failed the publish: %v", err)
	}
	if !removed {
		t.Fatal("the walk never reached the world, so this test asserts nothing")
	}
}
