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
// flavours accept, so every test below can start from something valid and
// then break exactly the one thing it is testing.
func writeConfigDir(t *testing.T, dir, secret string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, render.ValuesFile),
		[]byte("maxPlayers: 100\nplayerLimit: 500\n"), 0o644); err != nil {
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
	if !strings.Contains(msg, "paper") || !strings.Contains(msg, "velocity") {
		t.Errorf("stderr = %q, want it to name both valid flavours", msg)
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

// The asymmetry the brief warns about: Load returns the secret's content,
// which Paper needs and Velocity must never receive. Both parameters are
// string, so a swapped call at this wiring layer compiles cleanly and would
// only be caught by inspecting what actually landed in velocity.toml — which
// is what this test does, rather than trusting that Velocity's own tests
// (which only prove Velocity honours whatever path it is given) say anything
// about what main.go actually gives it.
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
