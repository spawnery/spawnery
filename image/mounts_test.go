/*
Copyright The Spawnery Authors.

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

package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entrypointRunner func(t *testing.T, workDir string, configExit int, env ...string) (string, error)

var bothFlavours = map[string]entrypointRunner{
	"paper":    runEntrypoint,
	"velocity": runVelocityEntrypoint,
}

// fakeMountinfo writes a mountinfo file listing each of points, relative to
// workDir, as a read-only mount point, escaped the way the kernel escapes them.
func fakeMountinfo(t *testing.T, workDir string, points ...string) string {
	t.Helper()
	return fakeMountinfoWith(t, workDir, "ro,relatime", points...)
}

func fakeMountinfoWith(t *testing.T, workDir, options string, points ...string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("22 1 0:21 / / rw,relatime - overlay overlay rw\n")
	for i, p := range points {
		escaped := strings.ReplaceAll(filepath.Join(real, p), " ", `\040`)
		fmt.Fprintf(&b, "%d 22 0:%d / %s %s - tmpfs tmpfs %s\n", 30+i, 40+i, escaped, options, options)
	}
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(b.String()), 0o444); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnExtraFilesPathUnderAMountRefusesTheStart(t *testing.T) {
	for flavour, run := range bothFlavours {
		t.Run(flavour, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(t.TempDir(), "files")
			writeTree(t, source, "mods/pack.jar", "config/sponge/sponge.conf")

			out, err := run(t, dir, 0,
				"SPAWNERY_FILE_SOURCE="+source,
				"SPAWNERY_MOUNTINFO="+fakeMountinfo(t, dir, "mods"))

			if err == nil {
				t.Fatalf("a claim carrying mods/ over a mount at mods started anyway:\n%s", out)
			}
			for _, want := range []string{"extraFiles", "mods", "spec.mounts"} {
				if !strings.Contains(out, want) {
					t.Errorf("the message does not name %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "Read-only file system") || strings.Contains(out, "cp:") {
				t.Errorf("the start died on the copy rather than on the scan:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(dir, "config", "sponge", "sponge.conf")); err == nil {
				t.Error("files were copied before the overlap was noticed")
			}
		})
	}
}

func TestAMountTheClaimDoesNotReachIsNoReasonToRefuse(t *testing.T) {
	for flavour, run := range bothFlavours {
		t.Run(flavour, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(t.TempDir(), "files")
			writeTree(t, source, "config/sponge/sponge.conf", "modsextra/a.txt")

			// modsextra shares a prefix with the mount and must not be read as
			// under it.
			out, err := run(t, dir, 0,
				"SPAWNERY_FILE_SOURCE="+source,
				"SPAWNERY_MOUNTINFO="+fakeMountinfo(t, dir, "mods", "config/other.yml"))

			if err != nil {
				t.Fatalf("refused a claim that overlaps no mount: %v\n%s", err, out)
			}
			for _, want := range []string{"config/sponge/sponge.conf", "modsextra/a.txt"} {
				if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
					t.Errorf("%s was not copied: %v", want, err)
				}
			}
		})
	}
}

func TestAPathWithASpaceIsReadBackFromMountinfo(t *testing.T) {
	for flavour, run := range bothFlavours {
		t.Run(flavour, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(t.TempDir(), "files")
			writeTree(t, source, "my maps/a.dat")

			out, err := run(t, dir, 0,
				"SPAWNERY_FILE_SOURCE="+source,
				"SPAWNERY_MOUNTINFO="+fakeMountinfo(t, dir, "my maps"))

			if err == nil {
				t.Fatalf("a claim carrying 'my maps' over a mount there started anyway:\n%s", out)
			}
			if !strings.Contains(out, "my maps") {
				t.Errorf("the message does not name the path:\n%s", out)
			}
		})
	}
}

func TestAnExtraPluginsPathUnderAMountRefusesTheStart(t *testing.T) {
	for flavour, run := range bothFlavours {
		t.Run(flavour, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(t.TempDir(), "plugins")
			writeTree(t, source, "LuckPerms.jar", "LuckPerms/config.yml")

			out, err := run(t, dir, 0,
				"SPAWNERY_PLUGIN_SOURCE="+source,
				"SPAWNERY_MOUNTINFO="+fakeMountinfo(t, dir, "plugins/LuckPerms/config.yml"))

			if err == nil {
				t.Fatalf("a plugin claim carrying a mounted file started anyway:\n%s", out)
			}
			for _, want := range []string{"extraPlugins", "LuckPerms/config.yml"} {
				if !strings.Contains(out, want) {
					t.Errorf("the message does not name %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestAMountBesideThePluginsIsNoReasonToRefuse(t *testing.T) {
	for flavour, run := range bothFlavours {
		t.Run(flavour, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(t.TempDir(), "plugins")
			writeTree(t, source, "LuckPerms.jar")

			out, err := run(t, dir, 0,
				"SPAWNERY_PLUGIN_SOURCE="+source,
				"SPAWNERY_MOUNTINFO="+fakeMountinfo(t, dir, "plugins/Other/config.yml", "LuckPerms.jar"))

			if err != nil {
				t.Fatalf("refused a plugin claim that overlaps no mount: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "plugins", "LuckPerms.jar")); err != nil {
				t.Errorf("the plugin was not copied: %v", err)
			}
		})
	}
}

// A writable claim mount takes the copy like any directory does.
func TestAWritableMountIsNoReasonToRefuse(t *testing.T) {
	for flavour, run := range bothFlavours {
		t.Run(flavour, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(t.TempDir(), "files")
			writeTree(t, source, "mods/pack.jar")

			out, err := run(t, dir, 0,
				"SPAWNERY_FILE_SOURCE="+source,
				"SPAWNERY_MOUNTINFO="+fakeMountinfoWith(t, dir, "rw,relatime", "mods"))

			if err != nil {
				t.Fatalf("refused a claim over a writable mount: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "mods", "pack.jar")); err != nil {
				t.Errorf("mods/pack.jar was not copied: %v", err)
			}
		})
	}
}
