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

package prune

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spawnery/spawnery/internal/sourcetree"
)

// claim writes files (a trailing slash makes an empty directory) under a new
// data directory.
func claim(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	put(t, dir, files...)
	return dir
}

func put(t *testing.T, dir string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(dir, f)
		if strings.HasSuffix(f, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// left lists every file below dir, relative, sorted.
func left(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func noMounts(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(dir string, keep []string, mountinfo string, pairs ...sourcetree.Pair) error {
	return Run(dir, keep, nil, mountinfo, pairs, io.Discard)
}

func runReplacing(dir string, keep, replace []string, mountinfo string, pairs ...sourcetree.Pair) error {
	return Run(dir, keep, replace, mountinfo, pairs, io.Discard)
}

// mountinfoWith writes a mount table holding one writable mount at rel below dir.
func mountinfoWith(t *testing.T, dir, rel string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "mountinfo")
	line := fmt.Sprintf("2 1 0:2 / %s rw,relatime - ext4 /dev/b rw\n", filepath.Join(root, rel))
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrune(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		keep  []string
		want  []string
	}{
		{
			name:  "unmatched files and directories go",
			files: []string{"server.properties", "logs/latest.log", "empty/", "world/level.dat"},
			keep:  []string{"world"},
			want:  []string{"world/level.dat"},
		},
		{
			name:  "a matched directory is kept whole",
			files: []string{"world/region/r.0.0.mca", "world/data/a.dat", "old/x"},
			keep:  []string{"world"},
			want:  []string{"world/data/a.dat", "world/region/r.0.0.mca"},
		},
		{
			name:  "a state directory survives beside files that go",
			files: []string{"plugins/ExampleGame/state/db.json", "plugins/ExampleGame/config.yml", "plugins/old.jar"},
			keep:  []string{"plugins/ExampleGame/state"},
			want:  []string{"plugins/ExampleGame/state/db.json"},
		},
		{
			name:  "globs match within a segment",
			files: []string{"world/level.dat", "world/level.dat_old", "world/session.lock", "world/dimensions/minecraft/map1/a", "world/dimensions/minecraft/other/b"},
			keep:  []string{"world/level.dat*", "world/dimensions/minecraft/map*"},
			want:  []string{"world/dimensions/minecraft/map1/a", "world/level.dat", "world/level.dat_old"},
		},
		{
			name:  "a glob in a middle segment",
			files: []string{"plugins/ExampleGame/state/x.json", "plugins/ExampleGame/config.yml"},
			keep:  []string{"plugins/*/state"},
			want:  []string{"plugins/ExampleGame/state/x.json"},
		},
		{
			name:  "a question mark is one character",
			files: []string{"a1/f", "a12/f"},
			keep:  []string{"a?"},
			want:  []string{"a1/f"},
		},
		{
			name:  "an empty claim is fine",
			files: nil,
			keep:  []string{"world"},
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := claim(t, tc.files...)
			if err := run(dir, tc.keep, noMounts(t)); err != nil {
				t.Fatal(err)
			}
			if got := left(t, dir); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("left %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLostAndFoundIsKeptAtTheRootOnly(t *testing.T) {
	dir := claim(t, "lost+found/orphan", "world/lost+found/x", "world/level.dat", "junk")
	if err := run(dir, []string{"world/level.dat"}, noMounts(t)); err != nil {
		t.Fatal(err)
	}
	if got := left(t, dir); fmt.Sprint(got) != "[lost+found/orphan world/level.dat]" {
		t.Errorf("left %v", got)
	}
}

func TestMountPointsAndTheirParentsAreKept(t *testing.T) {
	dir := claim(t, "mods/ro/a", "mods/other", "data/rw/b", "data/rw/deeper/c", "junk")
	root, _ := filepath.EvalSymlinks(dir)
	info := filepath.Join(t.TempDir(), "mountinfo")
	table := fmt.Sprintf("1 0 0:1 / / rw - overlay overlay rw\n"+
		"2 1 0:2 / %s/mods/ro ro,nosuid - ext4 /dev/a ro\n"+
		"3 1 0:3 / %s/data/rw rw,relatime - ext4 /dev/b rw\n", root, root)
	if err := os.WriteFile(info, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(dir, []string{"world"}, info); err != nil {
		t.Fatal(err)
	}
	want := "[data/rw/b data/rw/deeper/c mods/ro/a]"
	if got := left(t, dir); fmt.Sprint(got) != want {
		t.Errorf("left %v, want %s", got, want)
	}
}

func TestAMountPointWithASpaceIsUnescaped(t *testing.T) {
	dir := claim(t, "my data/a", "junk")
	root, _ := filepath.EvalSymlinks(dir)
	info := filepath.Join(t.TempDir(), "mountinfo")
	table := fmt.Sprintf("2 1 0:2 / %s/my\\040data rw - ext4 /dev/a rw\n", root)
	if err := os.WriteFile(info, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(dir, []string{"world"}, info); err != nil {
		t.Fatal(err)
	}
	if got := left(t, dir); fmt.Sprint(got) != "[my data/a]" {
		t.Errorf("left %v", got)
	}
}

func TestALevelDatNoEntryKeepsRefusesAndDeletesNothing(t *testing.T) {
	dir := claim(t, "junk", "world/level.dat", "old/world/level.dat")
	err := run(dir, []string{"world"}, noMounts(t))
	if err == nil || !strings.Contains(err.Error(), "old") {
		t.Fatalf("err = %v, want a refusal naming old", err)
	}
	if got := left(t, dir); len(got) != 3 {
		t.Errorf("deleted before refusing: %v", got)
	}
}

func TestASourceCarryingAKeptPathRefusesAndDeletesNothing(t *testing.T) {
	dir := claim(t, "junk", "plugins/ExampleGame/state/db.json")
	src := claim(t, "ExampleGame/state/seed.json", "ExampleGame/config.yml")
	err := run(dir, []string{"plugins/ExampleGame/state"}, noMounts(t), sourcetree.Pair{From: src, Into: "plugins"})
	if err == nil || !strings.Contains(err.Error(), "plugins/ExampleGame/state/seed.json") {
		t.Fatalf("err = %v, want a refusal naming the path", err)
	}
	if got := left(t, dir); len(got) != 2 {
		t.Errorf("deleted before refusing: %v", got)
	}
}

func TestASourceThatShipsOnlyUnkeptPathsIsFine(t *testing.T) {
	dir := claim(t, "junk")
	src := claim(t, "ExampleGame/config.yml", "lost+found/x")
	missing := filepath.Join(t.TempDir(), "absent")
	err := run(dir, []string{"plugins/ExampleGame/state"}, noMounts(t),
		sourcetree.Pair{From: src, Into: "plugins"}, sourcetree.Pair{From: missing, Into: "."})
	if err != nil {
		t.Fatal(err)
	}
}

func TestASymlinkIsRemovedAndItsTargetStays(t *testing.T) {
	outside := claim(t, "precious")
	dir := claim(t, "keep/a")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := run(dir, []string{"keep"}, noMounts(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Error("the link survived")
	}
	if got := left(t, outside); fmt.Sprint(got) != "[precious]" {
		t.Errorf("target changed: %v", got)
	}
}

func TestEachRemovalIsLogged(t *testing.T) {
	dir := claim(t, "keep/a", "plugins/old.jar")
	var log strings.Builder
	if err := Run(dir, []string{"keep"}, nil, noMounts(t), nil, &log); err != nil {
		t.Fatal(err)
	}
	if log.String() != "spawnery: keep: removing plugins\n" {
		t.Errorf("log = %q", log.String())
	}
}

func TestABadEntryRefuses(t *testing.T) {
	for _, k := range []string{"", "/x", "a/../b", "a//b", "a/./b", "a[b"} {
		if err := run(claim(t), []string{k}, "none"); err == nil {
			t.Errorf("entry %q was accepted", k)
		}
	}
}

func TestAMissingMountinfoRefusesAndDeletesNothing(t *testing.T) {
	dir := claim(t, "junk", "keep/a")
	if err := run(dir, []string{"keep"}, filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing mountinfo was accepted")
	}
	if got := left(t, dir); len(got) != 2 {
		t.Errorf("deleted before refusing: %v", got)
	}
}

func TestARelativeRootStillSeesAbsoluteMountPoints(t *testing.T) {
	dir := claim(t, "mods/ro/a", "data/rw/b", "junk")
	root, _ := filepath.EvalSymlinks(dir)
	info := filepath.Join(t.TempDir(), "mountinfo")
	table := fmt.Sprintf("2 1 0:2 / %s/mods/ro ro - ext4 /dev/a ro\n3 1 0:3 / %s/data/rw rw - ext4 /dev/b rw\n", root, root)
	if err := os.WriteFile(info, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := run(".", []string{"world"}, info); err != nil {
		t.Fatal(err)
	}
	want := "[data/rw/b mods/ro/a]"
	if got := left(t, dir); fmt.Sprint(got) != want {
		t.Errorf("left %v, want %s", got, want)
	}
}

func TestEveryShapeOfAWorldRefusesAndDeletesNothing(t *testing.T) {
	tests := map[string][]string{
		"rename leftovers only": {"old/world/level.dat_old", "old/world/level.dat_new"},
		"a bare region":         {"old/world/region/r.0.0.mca"},
		"an empty region":       {"old/world/region/"},
		"a stray mca":           {"old/r.0.0.mca"},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			dir := claim(t, append([]string{"keep/a"}, files...)...)
			err := run(dir, []string{"keep"}, noMounts(t))
			if err == nil || !strings.Contains(err.Error(), "old") {
				t.Fatalf("err = %v, want a refusal naming old", err)
			}
			for _, p := range []string{"keep/a", "old"} {
				if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
					t.Errorf("%s was deleted before refusing: %v", p, err)
				}
			}
		})
	}
}

func TestAnUnreadableSubtreeRefusesAndDeletesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := claim(t, "keep/a", "junk/x", "old/sealed/y")
	sealed := filepath.Join(dir, "old", "sealed")
	if err := os.Chmod(sealed, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })
	err := run(dir, []string{"keep"}, noMounts(t))
	if err == nil || !strings.Contains(err.Error(), "old") {
		t.Fatalf("err = %v, want a refusal naming old", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk", "x")); err != nil {
		t.Errorf("junk was deleted before refusing: %v", err)
	}
}

func TestAWorldASourceShipsIsDeletedWithoutRefusing(t *testing.T) {
	dir := claim(t,
		"worlds/world/level.dat", "worlds/world/region/r.0.0.mca",
		"worlds/world_templates/lobby/region/r.0.0.mca", "worlds/world_templates/lobby/level.dat",
		"plugins/X/internal/state", "logs/latest.log")
	src := claim(t, "worlds/world_templates/lobby/region/r.0.0.mca", "worlds/world_templates/lobby/level.dat")
	err := run(dir, []string{"worlds/world", "plugins/X/internal"}, noMounts(t), sourcetree.Pair{From: src, Into: "."})
	if err != nil {
		t.Fatal(err)
	}
	want := "[plugins/X/internal/state worlds/world/level.dat worlds/world/region/r.0.0.mca]"
	if got := left(t, dir); fmt.Sprint(got) != want {
		t.Errorf("left %v, want %s", got, want)
	}
}

func TestAWorldBesideWhatASourceShipsStillRefuses(t *testing.T) {
	tests := map[string]string{
		"a stale world from an older source":       "worlds/world_templates/oldlobby/region/r.0.0.mca",
		"an extra region file in a shipped one":    "worlds/world_templates/lobby/region/r.1.0.mca",
		"a level.dat the source does not ship":     "worlds/world_templates/lobby/level.dat",
		"an empty region the source does not ship": "worlds/world_templates/other/region/",
	}
	for name, extra := range tests {
		t.Run(name, func(t *testing.T) {
			dir := claim(t, "worlds/world/level.dat", "worlds/world_templates/lobby/region/r.0.0.mca", extra)
			src := claim(t, "worlds/world_templates/lobby/region/r.0.0.mca")
			err := run(dir, []string{"worlds/world"}, noMounts(t), sourcetree.Pair{From: src, Into: "."})
			if err == nil || !strings.Contains(err.Error(), "worlds/world_templates") {
				t.Fatalf("err = %v, want a refusal naming worlds/world_templates", err)
			}
			if _, err := os.Stat(filepath.Join(dir, extra)); err != nil {
				t.Errorf("deleted before refusing: %v", err)
			}
		})
	}
}

func TestAShippedPathCountsOnlyAtItsDestination(t *testing.T) {
	dir := claim(t, "keep/a", "plugins/Map/region/r.0.0.mca")
	src := claim(t, "Map/region/r.0.0.mca")
	if err := run(dir, []string{"keep"}, noMounts(t), sourcetree.Pair{From: src, Into: "."}); err == nil {
		t.Fatal("a world shipped to another destination was deleted without refusing")
	}
	if err := run(dir, []string{"keep"}, noMounts(t), sourcetree.Pair{From: src, Into: "plugins"}); err != nil {
		t.Fatal(err)
	}
}

func TestAWorldWhoseMarkersAreShippedButNotItsPlayerStateRefuses(t *testing.T) {
	shippedFiles := []string{"world/level.dat", "world/level.dat_old", "world/region/r.0.0.mca", "world/entities/r.0.0.mca"}
	dir := claim(t, append([]string{"plugins/X/c", "world/playerdata/0000-uuid.dat", "world/stats/0000-uuid.json", "world/data/scoreboard.dat"}, shippedFiles...)...)
	src := claim(t, shippedFiles...)
	err := run(dir, []string{"plugins/X"}, noMounts(t), sourcetree.Pair{From: src, Into: "."})
	if err == nil || !strings.Contains(err.Error(), "world") {
		t.Fatalf("err = %v, want a refusal naming world", err)
	}
	if got := left(t, dir); len(got) != 8 {
		t.Errorf("deleted before refusing: %v", got)
	}
}

func TestATopLevelNameStartingWithTwoDotsIsNotShipped(t *testing.T) {
	for _, files := range [][]string{{"..w/level.dat"}, {"..w/level.dat", "..w/playerdata/u.dat"}} {
		dir := claim(t, append([]string{"keep/a"}, files...)...)
		src := claim(t, "..w/level.dat")
		if err := run(dir, []string{"keep"}, noMounts(t), sourcetree.Pair{From: src, Into: "."}); err == nil {
			t.Fatalf("%v: a world the copy never writes was deleted without refusing", files)
		}
	}
}

func TestATopLevelLostAndFoundFileIsNotShipped(t *testing.T) {
	dir := claim(t, "plugins/lost+found/x")
	src := claim(t, "lost+found")
	if err := run(dir, []string{"plugins/lost+found"}, noMounts(t), sourcetree.Pair{From: src, Into: "plugins"}); err != nil {
		t.Fatalf("a file the copy skips counted as shipping a kept path: %v", err)
	}
}

func TestADanglingSymlinkASourceCarriesIsNotShipped(t *testing.T) {
	dir := claim(t, "plugins/S/db.json")
	src := claim(t)
	if err := os.Symlink(filepath.Join(src, "absent"), filepath.Join(src, "S")); err != nil {
		t.Fatal(err)
	}
	if err := run(dir, []string{"plugins/S"}, noMounts(t), sourcetree.Pair{From: src, Into: "plugins"}); err != nil {
		t.Fatalf("a link the copy skips counted as shipping a kept path: %v", err)
	}
	dir = claim(t, "keep/a", "w/level.dat")
	src = claim(t)
	if err := os.Symlink(filepath.Join(src, "absent"), filepath.Join(src, "w")); err != nil {
		t.Fatal(err)
	}
	if err := run(dir, []string{"keep"}, noMounts(t), sourcetree.Pair{From: src, Into: "."}); err == nil {
		t.Fatal("a world behind a link the copy skips was deleted without refusing")
	}
}

func TestAShippedEntryOfAnotherKindDoesNotExempt(t *testing.T) {
	tests := map[string]struct{ claim, source string }{
		"the claim has a file where the source ships a directory": {"w/x.mca", "w/x.mca/"},
		"the claim has a directory where the source ships a file": {"w/region/", "w/region"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := claim(t, "keep/a", tc.claim)
			src := claim(t, tc.source)
			if err := run(dir, []string{"keep"}, noMounts(t), sourcetree.Pair{From: src, Into: "."}); err == nil {
				t.Fatal("deleted without refusing")
			}
			if _, err := os.Stat(filepath.Join(dir, tc.claim)); err != nil {
				t.Errorf("deleted before refusing: %v", err)
			}
		})
	}
}

func TestAReplacedWorldIsDeletedEvenWithFilesNoSourceShips(t *testing.T) {
	dir := claim(t, "worlds/world/level.dat",
		"worlds/templates/lobby/region/r.0.0.mca", "worlds/templates/lobby/region/r.0.-2.mca")
	src := claim(t, "worlds/templates/lobby/region/r.0.0.mca")
	err := runReplacing(dir, []string{"worlds/world"}, []string{"worlds/templates"}, noMounts(t),
		sourcetree.Pair{From: src, Into: "."})
	if err != nil {
		t.Fatal(err)
	}
	if got := left(t, dir); fmt.Sprint(got) != "[worlds/world/level.dat]" {
		t.Errorf("left %v", got)
	}
}

func TestAWorldBesideAReplacedPathStillRefuses(t *testing.T) {
	dir := claim(t, "worlds/world/level.dat",
		"worlds/templates/lobby/region/r.0.0.mca", "worlds/other/region/r.0.0.mca")
	err := runReplacing(dir, []string{"worlds/world"}, []string{"worlds/templates"}, noMounts(t))
	if err == nil || !strings.Contains(err.Error(), "worlds/other") {
		t.Fatalf("err = %v, want a refusal naming worlds/other", err)
	}
	if got := left(t, dir); len(got) != 3 {
		t.Errorf("deleted before refusing: %v", got)
	}
}

func TestAReplaceGlobMatchesPerSegment(t *testing.T) {
	dir := claim(t, "world/level.dat", "worlds/templates/lobby/region/r.0.0.mca",
		"worlds/templates/arena/level.dat", "worlds/templates/readme")
	err := runReplacing(dir, []string{"world"}, []string{"worlds/templates/*"}, noMounts(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := left(t, dir); fmt.Sprint(got) != "[world/level.dat]" {
		t.Errorf("left %v", got)
	}
}

func TestKeepWinsOverReplace(t *testing.T) {
	dir := claim(t, "worlds/templates/lobby/level.dat", "worlds/templates/old/region/r.0.0.mca")
	err := runReplacing(dir, []string{"worlds/templates/lobby"}, []string{"worlds/templates"}, noMounts(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := left(t, dir); fmt.Sprint(got) != "[worlds/templates/lobby/level.dat]" {
		t.Errorf("left %v", got)
	}
}

func TestAMountUnderAReplacedPathSurvives(t *testing.T) {
	dir := claim(t, "world/level.dat", "worlds/templates/shared/region/r.0.0.mca", "worlds/templates/old/level.dat")
	err := runReplacing(dir, []string{"world"}, []string{"worlds/templates"},
		mountinfoWith(t, dir, "worlds/templates/shared"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[world/level.dat worlds/templates/shared/region/r.0.0.mca]"
	if got := left(t, dir); fmt.Sprint(got) != want {
		t.Errorf("left %v, want %s", got, want)
	}
}

func TestAReplaceEntryWithNothingToMatchChangesNothing(t *testing.T) {
	dir := claim(t, "world/level.dat", "old/world/level.dat")
	err := runReplacing(dir, []string{"world"}, []string{"worlds/templates"}, noMounts(t))
	if err == nil || !strings.Contains(err.Error(), "old") {
		t.Fatalf("err = %v, want the usual refusal naming old", err)
	}
}

func TestABadReplaceEntryRefusesAndNamesTheField(t *testing.T) {
	for _, r := range []string{"", "/x", "a//b", "a[b"} {
		err := runReplacing(claim(t), []string{"world"}, []string{r}, "none")
		if err == nil || !strings.Contains(err.Error(), "spec.storage.replace") {
			t.Errorf("entry %q: err = %v, want a refusal naming spec.storage.replace", r, err)
		}
	}
}

func TestAReplaceEntryInsideAnUnkeptWorldDoesNotOpenIt(t *testing.T) {
	dir := claim(t, "world/level.dat", "world/region/r.0.0.mca", "world/playerdata/u.dat",
		"world/stats/u.json", "world/datapacks/old.zip")
	src := claim(t, "world/level.dat", "world/region/r.0.0.mca", "world/datapacks/new.zip")
	err := runReplacing(dir, []string{"plugins/x/state"}, []string{"world/datapacks"}, noMounts(t),
		sourcetree.Pair{From: src, Into: "."})
	if err == nil || !strings.Contains(err.Error(), "world/") {
		t.Fatalf("err = %v, want a refusal naming a path of world", err)
	}
	if got := left(t, dir); len(got) != 5 {
		t.Errorf("deleted before refusing: %v", got)
	}
}

func TestAReplaceEntryAloneLeadsTheWalk(t *testing.T) {
	dir := claim(t, "world/level.dat", "worlds/templates/lobby/region/r.0.0.mca", "worlds/other/region/r.0.0.mca")
	err := runReplacing(dir, []string{"world"}, []string{"worlds/templates"}, noMounts(t))
	if err == nil || !strings.Contains(err.Error(), "worlds/other") {
		t.Fatalf("err = %v, want a refusal naming worlds/other, not all of worlds", err)
	}
}

func TestAReplaceEntryBelowAKeptPathLeavesItAlone(t *testing.T) {
	dir := claim(t, "world/level.dat", "world/datapacks/x.zip")
	if err := runReplacing(dir, []string{"world"}, []string{"world/datapacks"}, noMounts(t)); err != nil {
		t.Fatal(err)
	}
	if got := left(t, dir); fmt.Sprint(got) != "[world/datapacks/x.zip world/level.dat]" {
		t.Errorf("left %v", got)
	}
}

func TestTheWorldRefusalPointsToReplace(t *testing.T) {
	dir := claim(t, "world/level.dat", "old/world/level.dat")
	err := run(dir, []string{"world"}, noMounts(t))
	if err == nil || !strings.Contains(err.Error(), "spec.storage.replace") {
		t.Fatalf("err = %v, want the refusal to name spec.storage.replace as the remedy", err)
	}
}
