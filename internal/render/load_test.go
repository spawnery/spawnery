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

package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is the fixture helper every test below shares: it creates parent
// directories and writes content, failing the test immediately if either
// step fails, since a fixture that cannot be built makes the rest of the
// test meaningless.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
}

// validConfig and validSecret are the two inputs every test below writes
// unless the test is specifically about that input's absence — so each test
// isolates the one refusal it names rather than tripping over an unrelated
// one.
const validConfig = "maxPlayers: 100\n"
const validSecret = "s3cret\n"

func TestLoadRefusesAMissingValuesFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, SecretFile), validSecret)

	_, _, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a directory with no config.yaml")
	}
	if !strings.Contains(err.Error(), ValuesFile) {
		t.Errorf("error = %q, want it to name %s", err, ValuesFile)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to say the file was not found, not something else — "+
			"an unparseable file and a missing one must read differently", err)
	}
}

func TestLoadRefusesAValuesFileThatDoesNotParse(t *testing.T) {
	dir := t.TempDir()
	// maxPlayers is *int32; a string scalar there is a YAML document that
	// parses on its own but fails to convert into Values, which is the
	// failure this test is pinned to — not a YAML syntax error, which a
	// less careful fixture could produce by accident.
	writeFile(t, filepath.Join(dir, ValuesFile), "maxPlayers: not-a-number\n")
	writeFile(t, filepath.Join(dir, SecretFile), validSecret)

	_, _, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a config.yaml that cannot become a Values")
	}
	if !strings.Contains(err.Error(), ValuesFile) {
		t.Errorf("error = %q, want it to name %s", err, ValuesFile)
	}
	if !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("error = %q, want it to say the file does not parse, not something else — "+
			"a missing file and an unparseable one must read differently", err)
	}
}

func TestLoadRefusesAMissingSecretFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)

	_, _, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a directory with no forwarding secret")
	}
	if !strings.Contains(err.Error(), SecretFile) {
		t.Errorf("error = %q, want it to name %s", err, SecretFile)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to say the file was not found, not something else — "+
			"an empty secret and a missing one must read differently", err)
	}
}

func TestLoadRefusesAnEmptySecretFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)
	// Whitespace only, not literally zero bytes: Load trims before checking,
	// so a secret file containing only a trailing newline — the shape a
	// Secret volume mount or `echo` into a file actually produces — must
	// refuse exactly the same way an empty file does.
	writeFile(t, filepath.Join(dir, SecretFile), "   \n")

	_, _, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a forwarding secret that is blank once trimmed")
	}
	if !strings.Contains(err.Error(), SecretFile) {
		t.Errorf("error = %q, want it to name %s", err, SecretFile)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want it to say the secret is empty, not something else — "+
			"a missing file and an empty one must read differently", err)
	}
}

func TestLoadRefusesAnUnreadableOverlayEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not block reads")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)
	writeFile(t, filepath.Join(dir, SecretFile), validSecret)

	overlayPath := filepath.Join(dir, OverlayDir, "server.properties")
	writeFile(t, overlayPath, "motd=hi\n")
	if err := os.Chmod(overlayPath, 0o000); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(overlayPath, 0o644) })

	_, _, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted an overlay entry it could not read")
	}
	if !strings.Contains(err.Error(), "server.properties") {
		t.Errorf("error = %q, want it to name the overlay file", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q, want it to say why the read failed, not something else — "+
			"this must not be confused with a missing or malformed overlay", err)
	}
}

// A missing overlay directory is not a refusal: the overlay is optional, and
// most Servers will never have one.
func TestLoadTreatsAMissingOverlayDirectoryAsOptional(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)
	writeFile(t, filepath.Join(dir, SecretFile), validSecret)

	_, _, overlay, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(overlay) != 0 {
		t.Errorf("overlay = %v, want empty when overlay/ does not exist", overlay)
	}
}

// The happy path: every value Load reads reaches the caller, and the secret
// arrives trimmed since Paper and Velocity both refuse an empty one and a
// trailing newline must not count as content.
func TestLoadReadsValuesSecretAndOverlay(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), "maxPlayers: 100\nmotd: hello\n")
	writeFile(t, filepath.Join(dir, SecretFile), "s3cret\n")
	writeFile(t, filepath.Join(dir, OverlayDir, "server.properties"), "difficulty=hard\n")

	v, secret, overlay, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v.MaxPlayers == nil || *v.MaxPlayers != 100 {
		t.Errorf("MaxPlayers = %v, want 100", v.MaxPlayers)
	}
	if v.Motd == nil || *v.Motd != "hello" {
		t.Errorf("Motd = %v, want %q", v.Motd, "hello")
	}
	if secret != "s3cret" {
		t.Errorf("secret = %q, want it trimmed to %q", secret, "s3cret")
	}
	if overlay["server.properties"] != "difficulty=hard\n" {
		t.Errorf("overlay[server.properties] = %q, want %q", overlay["server.properties"], "difficulty=hard\n")
	}
}

// mountConfigMapStyleOverlay lays overlayDir out the way the kubelet actually
// lays out a mounted ConfigMap or Secret directory — the idiom
// internal/podspec already uses for the agent CA and for a Server's own
// mounts, not an exotic case: a hidden, timestamped directory holds the real
// files, "..data" is a symlink to that directory, and each key is a symlink
// through "..data" rather than a regular file. A t.TempDir() fixture of
// plain regular files, as every other test in this file uses, would never
// exercise this shape and would never have caught loadOverlay filtering on
// DirEntry.Type().IsRegular(), which is false for all three of these entry
// kinds and used to return an empty overlay with no error.
func mountConfigMapStyleOverlay(t *testing.T, overlayDir string, files map[string]string) {
	t.Helper()
	const dataDir = "..2024_01_01_00_00_00.000000000"

	for name, content := range files {
		writeFile(t, filepath.Join(overlayDir, dataDir, name), content)
	}
	if err := os.Symlink(dataDir, filepath.Join(overlayDir, "..data")); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for name := range files {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(overlayDir, name)); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
}

// The regression this task's review caught: a real kubelet-mounted overlay
// directory is symlinks, not regular files, and loadOverlay must resolve
// them rather than filter them out.
func TestLoadReadsAKubeletMountedOverlay(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)
	writeFile(t, filepath.Join(dir, SecretFile), validSecret)
	mountConfigMapStyleOverlay(t, filepath.Join(dir, OverlayDir), map[string]string{
		"server.properties": "difficulty=hard\n",
	})

	_, _, overlay, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if overlay["server.properties"] != "difficulty=hard\n" {
		t.Errorf("overlay[server.properties] = %q, want %q — "+
			"a kubelet-mounted overlay must not read back as empty",
			overlay["server.properties"], "difficulty=hard\n")
	}
	if len(overlay) != 1 {
		t.Errorf("overlay = %v, want exactly one key — "+
			"\"..data\" and the hidden timestamped directory must not appear as overlay entries", overlay)
	}
}
