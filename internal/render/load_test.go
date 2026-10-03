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

package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
}

// Every test writes both unless it is about that input's absence.
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
	// Valid YAML that fails to convert into Values, not a syntax error.
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
	// Whitespace only, the shape a Secret mount or `echo` produces.
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
		t.Errorf("secret = %q, want %q", secret, "s3cret")
	}
	if overlay["server.properties"] != "difficulty=hard\n" {
		t.Errorf("overlay[server.properties] = %q, want %q", overlay["server.properties"], "difficulty=hard\n")
	}
}

// `head -c 32 /dev/urandom | base64`, as config/samples/network.yaml
// documents, leaves a trailing newline that Velocity's own read drops too.
func TestLoadAcceptsASecretWithATrailingNewline(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)
	writeFile(t, filepath.Join(dir, SecretFile), "s3cret\n")

	_, secret, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load refused a secret with only a trailing newline: %v", err)
	}
	if secret != "s3cret" {
		t.Errorf("secret = %q, want %q — the trailing newline must be stripped, "+
			"not merely tolerated", secret, "s3cret")
	}
}

func TestLoadAcceptsASecretWithATrailingCRLF(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)
	writeFile(t, filepath.Join(dir, SecretFile), "s3cret\r\n")

	_, secret, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load refused a secret with only a trailing CRLF: %v", err)
	}
	if secret != "s3cret" {
		t.Errorf("secret = %q, want %q — the trailing CRLF must be stripped as one terminator, "+
			"not just the final \\n", secret, "s3cret")
	}
}

// Carries a legal trailing newline too, so only the leading spaces can cause
// the refusal.
func TestLoadRefusesASecretWithLeadingSpaces(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)
	writeFile(t, filepath.Join(dir, SecretFile), "  s3cret\n")

	_, _, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a forwarding secret with leading spaces")
	}
	if !strings.Contains(err.Error(), SecretFile) {
		t.Errorf("error = %q, want it to name %s", err, SecretFile)
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("error = %q, want it to say the secret carries surrounding whitespace, not something else — "+
			"an empty secret and an interior line break must read differently", err)
	}
}

func TestLoadRefusesASecretWithTrailingTabs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)
	writeFile(t, filepath.Join(dir, SecretFile), "s3cret\t\t\n")

	_, _, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a forwarding secret with trailing tabs")
	}
	if !strings.Contains(err.Error(), SecretFile) {
		t.Errorf("error = %q, want it to name %s", err, SecretFile)
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("error = %q, want it to say the secret carries surrounding whitespace, not something else — "+
			"an empty secret and an interior line break must read differently", err)
	}
}

// Only the interior newline can be the cause once the trailing one is
// stripped.
func TestLoadRefusesASecretWithAnInteriorNewline(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ValuesFile), validConfig)
	writeFile(t, filepath.Join(dir, SecretFile), "s3\ncret\n")

	_, _, _, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a forwarding secret with an interior newline")
	}
	if !strings.Contains(err.Error(), SecretFile) {
		t.Errorf("error = %q, want it to name %s", err, SecretFile)
	}
	if !strings.Contains(err.Error(), "interior line break") {
		t.Errorf("error = %q, want it to say the secret contains an interior line break, not something else — "+
			"an empty secret and surrounding whitespace must read differently", err)
	}
}

// mountConfigMapStyleOverlay lays files out the way the kubelet mounts a
// ConfigMap: a hidden timestamped directory, a "..data" symlink to it, and
// each key a symlink through "..data". None of these is a regular file.
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
