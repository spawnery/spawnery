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

// Package image holds tests for the shell parts of the Paper base image. There
// is no Go code here to build — the entrypoint is a shell script, and this is
// how its rules stay provable in make test rather than only in a container.
package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

// stubJava puts a fake java on PATH that prints its arguments instead of
// starting a JVM. The entrypoint ends in exec, so whatever it prints is the
// command line the image would really run.
func stubJava(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'JAVA_ARGV: %s\\n' \"$*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "java"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub java: %v", err)
	}
	return dir
}

// runEntrypoint runs the script in workDir and returns its combined output.
func runEntrypoint(t *testing.T, workDir string, env ...string) (string, error) {
	t.Helper()
	script := testenv.RepoPath(t, "image/entrypoint.sh")

	cmd := exec.Command("sh", script)
	cmd.Dir = workDir
	cmd.Env = append([]string{
		"PATH=" + stubJava(t) + ":" + os.Getenv("PATH"),
		"PAPER_HOME=/opt/paper",
	}, env...)

	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestEntrypointAcceptsTheEula(t *testing.T) {
	dir := t.TempDir()

	if _, err := runEntrypoint(t, dir, "SPAWNERY_MAX_PLAYERS=100"); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	eula, err := os.ReadFile(filepath.Join(dir, "eula.txt"))
	if err != nil {
		t.Fatalf("read eula.txt: %v", err)
	}
	if strings.TrimSpace(string(eula)) != "eula=true" {
		t.Errorf("eula.txt is %q, want %q", string(eula), "eula=true")
	}
}

func TestEntrypointEnforcesTheOperationalFieldsAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()

	// What a user mount might have put there: two settings of their own, and
	// two the operator has to be able to rely on, set wrongly.
	existing := "motd=Hello\nview-distance=6\nmax-players=20\nenable-status=false\n"
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(existing), 0o644); err != nil {
		t.Fatalf("write server.properties: %v", err)
	}

	if _, err := runEntrypoint(t, dir, "SPAWNERY_MAX_PLAYERS=100"); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read server.properties: %v", err)
	}
	props := parseProperties(string(raw))

	enforced := map[string]string{
		"server-port":   "25565",
		"max-players":   "100",
		"enable-status": "true",
	}
	for key, want := range enforced {
		if got := props[key]; got != want {
			t.Errorf("%s is %q, want %q — the operator relies on this one", key, got, want)
		}
	}

	kept := map[string]string{"motd": "Hello", "view-distance": "6"}
	for key, want := range kept {
		if got := props[key]; got != want {
			t.Errorf("%s is %q, want %q — user settings must survive", key, got, want)
		}
	}

	// No key may appear twice; Paper would take the last one and the file
	// would drift further apart on every restart.
	seen := map[string]int{}
	for _, line := range strings.Split(string(raw), "\n") {
		if key, _, ok := strings.Cut(line, "="); ok {
			seen[key]++
		}
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("%s appears %d times, want once", key, count)
		}
	}
}

func TestEntrypointExecsJavaWithTheBundlerRepo(t *testing.T) {
	dir := t.TempDir()

	out, err := runEntrypoint(t, dir, "SPAWNERY_MAX_PLAYERS=100")
	if err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	for _, want := range []string{
		"JAVA_ARGV:",
		"-DbundlerRepoDir=/opt/paper/repo",
		"-jar /opt/paper/paper.jar",
		"--nogui",
		"-XX:MaxRAMPercentage=75",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("java was not invoked with %q; got: %s", want, out)
		}
	}
}

func TestEntrypointRefusesAnUnusableMaxPlayers(t *testing.T) {
	tests := []struct {
		name string
		env  []string
	}{
		{name: "unset", env: nil},
		{name: "not a number", env: []string{"SPAWNERY_MAX_PLAYERS=lots"}},
		{name: "empty", env: []string{"SPAWNERY_MAX_PLAYERS="}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runEntrypoint(t, t.TempDir(), tt.env...)
			if err == nil {
				t.Fatalf("entrypoint succeeded, want a failure; output: %s", out)
			}
			if strings.Contains(out, "JAVA_ARGV:") {
				t.Errorf("java was started anyway; output: %s", out)
			}
		})
	}
}

func TestCopiesTheAgentPluginIntoAWritablePluginsDirectory(t *testing.T) {
	dir := t.TempDir()

	// The image ships the jar in the read-only part; the entrypoint's job is
	// to get it somewhere Paper may also write, because Paper puts its
	// plugins' data folders inside the plugins directory.
	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(paperHome, "agent", "spawnery-agent.jar")
	if err := os.WriteFile(jar, []byte("fresh"), 0o444); err != nil {
		t.Fatal(err)
	}

	// A stale copy from a previous start must lose: the image is the truth.
	if err := os.MkdirAll(filepath.Join(dir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "plugins", "spawnery-agent.jar")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, "SPAWNERY_MAX_PLAYERS=100", "PAPER_HOME="+paperHome); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("the agent jar is not in the plugins directory: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("plugins/spawnery-agent.jar = %q, want the copy from the image", got)
	}
}

func parseProperties(raw string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			props[key] = value
		}
	}
	return props
}
