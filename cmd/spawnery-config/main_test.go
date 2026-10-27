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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawnery/spawnery/internal/render"
)

// writeConfigDir builds a config directory that both Load and the two
// flavours accept.
func writeConfigDir(t *testing.T, dir, secret string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, render.ValuesFile),
		[]byte("maxPlayers: 100\nplayerLimit: 500\nonlineMode: true\n"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, render.SecretFile), []byte(secret), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
}

func TestRunRejectsAnUnknownFlavor(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"--flavor", "bungeecord"}, &stderr)

	if code != 2 {
		t.Errorf("exit code is %d, want 2 for a usage error", code)
	}
	msg := stderr.String()
	for _, flavor := range []string{"paper", "canvas", "velocity"} {
		if !strings.Contains(msg, flavor) {
			t.Errorf("stderr = %q, want it to name the flavour %s", msg, flavor)
		}
	}
}

func TestRunFailsWhenLoadRefuses(t *testing.T) {
	dir := t.TempDir()
	// No config.yaml written: Load must refuse before either flavour runs.

	var stderr bytes.Buffer
	code := run([]string{"--flavor", "paper", "--config-dir", dir, "--out", t.TempDir()}, &stderr)

	if code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), render.ValuesFile) {
		t.Errorf("stderr = %q, want it to name %s", stderr.String(), render.ValuesFile)
	}
}

func TestRunRendersPaperToOut(t *testing.T) {
	configDir := t.TempDir()
	writeConfigDir(t, configDir, "s3cret")
	out := t.TempDir()

	var stderr bytes.Buffer
	code := run([]string{"--flavor", "paper", "--config-dir", configDir, "--out", out}, &stderr)

	if code != 0 {
		t.Fatalf("exit code is %d, want 0; stderr: %s", code, stderr.String())
	}
	props, err := os.ReadFile(filepath.Join(out, "server.properties"))
	if err != nil {
		t.Fatalf("server.properties: %v", err)
	}
	if !strings.Contains(string(props), "online-mode=false") {
		t.Errorf("server.properties = %q, want online-mode=false", props)
	}
	global, err := os.ReadFile(filepath.Join(out, "config", "paper-global.yml"))
	if err != nil {
		t.Fatalf("config/paper-global.yml: %v", err)
	}
	if !strings.Contains(string(global), "s3cret") {
		t.Errorf("paper-global.yml = %q, want it to carry the secret's content", global)
	}
}

func TestRunRendersCanvasWithItsOwnKeys(t *testing.T) {
	configDir := t.TempDir()
	writeConfigDir(t, configDir, "s3cret")
	overlayDir := filepath.Join(configDir, render.OverlayDir)
	if err := os.Mkdir(overlayDir, 0o755); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "paper-global.yml"),
		[]byte("threaded-regions:\n  threads: 4\n"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	for flavor, wantCode := range map[string]int{"canvas": 0, "paper": 1} {
		out := t.TempDir()
		var stderr bytes.Buffer
		code := run([]string{"--flavor", flavor, "--config-dir", configDir, "--out", out}, &stderr)
		if code != wantCode {
			t.Errorf("--flavor %s: exit code is %d, want %d; stderr: %s", flavor, code, wantCode, stderr.String())
		}
		if flavor != "canvas" {
			continue
		}
		global, err := os.ReadFile(filepath.Join(out, "config", "paper-global.yml"))
		if err != nil {
			t.Fatalf("config/paper-global.yml: %v", err)
		}
		if !strings.Contains(string(global), "threads: 4") {
			t.Errorf("paper-global.yml = %q, want threaded-regions.threads", global)
		}
	}
}

// Load returns the secret's content, which Paper needs and Velocity must never
// receive. Both are strings, so a swapped call compiles.
func TestRunWiresTheSecretPathNotItsContentIntoVelocity(t *testing.T) {
	configDir := t.TempDir()
	writeConfigDir(t, configDir, "s3cret-content-must-not-leak")
	out := t.TempDir()

	var stderr bytes.Buffer
	code := run([]string{"--flavor", "velocity", "--config-dir", configDir, "--out", out}, &stderr)

	if code != 0 {
		t.Fatalf("exit code is %d, want 0; stderr: %s", code, stderr.String())
	}
	toml, err := os.ReadFile(filepath.Join(out, "velocity.toml"))
	if err != nil {
		t.Fatalf("velocity.toml: %v", err)
	}
	rendered := string(toml)

	wantPath := filepath.Join(configDir, render.SecretFile)
	if !strings.Contains(rendered, wantPath) {
		t.Errorf("velocity.toml = %q, want forwarding-secret-file pointing at %s", rendered, wantPath)
	}
	if strings.Contains(rendered, "s3cret-content-must-not-leak") {
		t.Error("velocity.toml carries the secret's content; main.go passed Load's secret content " +
			"to Velocity instead of the secret's path")
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"--nonsense"}, &stderr)

	if code != 2 {
		t.Errorf("exit code is %d, want 2 for a usage error", code)
	}
}

func TestSubstituteFillsFromTheEnvironment(t *testing.T) {
	from, into := t.TempDir(), t.TempDir()
	for _, root := range []string{from, into} {
		if err := os.WriteFile(filepath.Join(root, "c.yml"), []byte("p: {{ SECRET_P }}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SECRET_P", "s3cret")
	var stderr bytes.Buffer
	if code := run([]string{"--substitute", "SECRET_", "--pair", from + "=" + into}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	b, _ := os.ReadFile(filepath.Join(into, "c.yml"))
	if string(b) != "p: s3cret\n" {
		t.Errorf("got %q", b)
	}
}

func TestSubstituteFailsWithoutLeakingAValue(t *testing.T) {
	from, into := t.TempDir(), t.TempDir()
	for _, root := range []string{from, into} {
		if err := os.WriteFile(filepath.Join(root, "c.yml"), []byte("{{ SECRET_A }} {{ SECRET_B }}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SECRET_A", "hunter2")
	var stderr bytes.Buffer
	if code := run([]string{"--substitute", "SECRET_", "--pair", from + "=" + into}, &stderr); code == 0 {
		t.Fatal("a missing variable exited 0")
	}
	if !strings.Contains(stderr.String(), "SECRET_B") || strings.Contains(stderr.String(), "hunter2") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func emptyMountinfo(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPruneNeedsTheKeepEntries(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"--prune"}, &stderr); code != 2 {
		t.Errorf("exit code is %d, want 2 for a usage error", code)
	}
}

func TestPruneDeletesWhatIsNotKept(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"keep/a", "junk/b"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	var stderr bytes.Buffer
	if code := run([]string{"--prune", "keep", "--mountinfo", emptyMountinfo(t)}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "junk")); err == nil {
		t.Error("junk survived")
	}
	if _, err := os.Stat(filepath.Join(dir, "keep", "a")); err != nil {
		t.Error("keep/a is gone")
	}
}

func TestPruneRefusesALevelDatItWouldDelete(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old", "level.dat"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var stderr bytes.Buffer
	if code := run([]string{"--prune", "world", "--mountinfo", emptyMountinfo(t)}, &stderr); code != 1 {
		t.Errorf("exit code is %d, want 1 for a refusal", code)
	}
	if !strings.Contains(stderr.String(), "old/level.dat") && !strings.Contains(stderr.String(), "old") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestPruneReplaceSkipsTheWorldGuard(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"keep/a", "old/level.dat"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	var stderr bytes.Buffer
	if code := run([]string{"--prune", "keep", "--mountinfo", emptyMountinfo(t), "--replace", "old"}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); err == nil {
		t.Error("old survived")
	}
}

func TestAnEmptyReplaceIsNoReplace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old", "level.dat"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var stderr bytes.Buffer
	if code := run([]string{"--prune", "world", "--mountinfo", emptyMountinfo(t), "--replace", ""}, &stderr); code != 1 {
		t.Errorf("exit code is %d, want 1: an empty --replace must not exempt anything", code)
	}
}

func renderVelocity(t *testing.T, values string) (string, string) {
	t.Helper()
	configDir := t.TempDir()
	writeConfigDir(t, configDir, "s3cret")
	if err := os.WriteFile(filepath.Join(configDir, render.ValuesFile), []byte(values), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	out := t.TempDir()
	var stderr bytes.Buffer
	if code := run([]string{"--flavor", "velocity", "--config-dir", configDir, "--out", out}, &stderr); code != 0 {
		t.Fatalf("exit code is %d, want 0; stderr: %s", code, stderr.String())
	}
	toml, err := os.ReadFile(filepath.Join(out, "velocity.toml"))
	if err != nil {
		t.Fatalf("velocity.toml: %v", err)
	}
	return string(toml), stderr.String()
}

// The pod's environment is what its agent reads; config.yaml follows the
// group, which a roll has already changed when an old pod's container restarts.
func TestVelocityAcceptsTransfersAsThePodWasCreated(t *testing.T) {
	const values = "playerLimit: 500\nonlineMode: true\n"

	t.Setenv(render.EnvTransfer, "120")
	toml, stderr := renderVelocity(t, values)
	if !strings.Contains(toml, "accepts-transfers = true") {
		t.Errorf("a pod created with transfer on rendered %q", toml)
	}
	if !strings.Contains(stderr, "following the pod") {
		t.Errorf("the disagreement went unreported; stderr: %q", stderr)
	}

	if err := os.Unsetenv(render.EnvTransfer); err != nil {
		t.Fatalf("unset %s: %v", render.EnvTransfer, err)
	}
	toml, _ = renderVelocity(t, values+"acceptsTransfers: true\n")
	if !strings.Contains(toml, "accepts-transfers = false") {
		t.Errorf("a pod created with transfer off rendered %q", toml)
	}
}
