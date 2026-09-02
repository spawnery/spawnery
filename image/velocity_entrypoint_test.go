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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runVelocityEntrypoint runs image/velocity-entrypoint.sh through the same
// stub harness runEntrypoint uses for Paper's — the harness only works here
// because the script calls spawnery-config unqualified, the same way
// image/entrypoint.sh does, rather than by the hardcoded
// /usr/local/bin/spawnery-config it used to carry. VELOCITY_HOME defaults to
// /opt/velocity, overridable through env like any other variable runScript
// passes.
func runVelocityEntrypoint(t *testing.T, workDir string, configExit int, env ...string) (string, error) {
	t.Helper()
	return runScript(t, "image/velocity-entrypoint.sh", workDir, configExit,
		append([]string{"VELOCITY_HOME=/opt/velocity"}, env...)...)
}

// TestVelocityEntrypointInvokesSpawneryConfigWithTheVelocityFlavor is the
// mirror of TestEntrypointInvokesSpawneryConfigWithThePaperFlavor, and the
// reason both need to exist rather than just one: the two scripts are nearly
// identical shell, and the plausible copy-paste mistake in either direction —
// this file's own namesake risk — is passing the wrong --flavor. What
// spawnery-config does with that flag is internal/render's and
// cmd/spawnery-config's coverage, not this package's.
func TestVelocityEntrypointInvokesSpawneryConfigWithTheVelocityFlavor(t *testing.T) {
	dir := t.TempDir()

	out, err := runVelocityEntrypoint(t, dir, 0)
	if err != nil {
		t.Fatalf("velocity entrypoint: %v", err)
	}
	if !strings.Contains(out, "SPAWNERY_CONFIG_ARGV: --flavor velocity") {
		t.Errorf("spawnery-config was not invoked with --flavor velocity; got: %s", out)
	}
}

func TestVelocityEntrypointExecsJavaWithTheVelocityJar(t *testing.T) {
	dir := t.TempDir()

	out, err := runVelocityEntrypoint(t, dir, 0)
	if err != nil {
		t.Fatalf("velocity entrypoint: %v", err)
	}

	for _, want := range []string{
		"JAVA_ARGV:",
		"-jar /opt/velocity/velocity.jar",
		"-XX:MaxRAMPercentage=75",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("java was not invoked with %q; got: %s", want, out)
		}
	}
}

// TestVelocityEntrypointStopsIfSpawneryConfigRefuses is the property
// TestEntrypointStopsIfSpawneryConfigRefuses already proves for Paper, and
// the one this file exists most for: a Velocity proxy that starts against
// wrong or missing forwarding configuration does not merely serve one broken
// backend, it makes every join on the network fail with "Unable to verify
// player details" while looking like a running proxy. The JVM must never
// start once the renderer has refused.
func TestVelocityEntrypointStopsIfSpawneryConfigRefuses(t *testing.T) {
	dir := t.TempDir()

	velocityHome := filepath.Join(dir, "opt", "velocity")
	if err := os.MkdirAll(filepath.Join(velocityHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(velocityHome, "agent", "spawnery-agent.jar"), []byte("fresh"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runVelocityEntrypoint(t, dir, 1, "VELOCITY_HOME="+velocityHome)
	if err == nil {
		t.Fatalf("velocity entrypoint succeeded, want a failure; output: %s", out)
	}

	if !strings.Contains(out, "SPAWNERY_CONFIG_ARGV: --flavor velocity") {
		t.Errorf("spawnery-config was never invoked; output: %s", out)
	}
	if strings.Contains(out, "JAVA_ARGV:") {
		t.Errorf("java was started anyway; output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "spawnery-agent.jar")); err == nil {
		t.Error("the agent plugin was copied anyway, after the renderer refused")
	}
}

// TestVelocityEntrypointCopiesTheAgentPluginIntoAWritablePluginsDirectory
// mirrors the Paper case: the jar ships in the read-only image and the
// entrypoint's job is to get it somewhere Velocity may also write, replacing
// whatever a previous start left there — the image is the truth.
func TestVelocityEntrypointCopiesTheAgentPluginIntoAWritablePluginsDirectory(t *testing.T) {
	dir := t.TempDir()

	velocityHome := filepath.Join(dir, "opt", "velocity")
	if err := os.MkdirAll(filepath.Join(velocityHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(velocityHome, "agent", "spawnery-agent.jar")
	if err := os.WriteFile(jar, []byte("fresh"), 0o444); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "plugins", "spawnery-agent.jar")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runVelocityEntrypoint(t, dir, 0, "VELOCITY_HOME="+velocityHome); err != nil {
		t.Fatalf("velocity entrypoint: %v", err)
	}

	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("the agent jar is not in the plugins directory: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("plugins/spawnery-agent.jar = %q, want the copy from the image", got)
	}
}

// TestTheVelocityJVMDoesNotPreTouchWithoutAMemoryLimit is the mirror of the
// Paper test, and needs to exist for the reason every mirrored test in this
// file does: the two scripts are nearly identical and diverge silently. A
// proxy holds every player's connection, so a proxy that claims three quarters
// of an unbounded node at start takes the whole network's front door with it.
func TestTheVelocityJVMDoesNotPreTouchWithoutAMemoryLimit(t *testing.T) {
	out, err := runVelocityEntrypoint(t, t.TempDir(), 0, cgroupRoot(t, "max", false))
	if err != nil {
		t.Fatalf("entrypoint: %v", err)
	}
	argv := javaArgv(t, out)
	if strings.Contains(argv, "AlwaysPreTouch") {
		t.Errorf("the proxy JVM pre-touches its heap with no memory limit; got: %s", argv)
	}
	if !strings.Contains(argv, "-XX:MaxRAMPercentage=75") {
		t.Errorf("the rest of the flags went with it; got: %s", argv)
	}
}

// TestTheVelocityJVMStillPreTouchesUnderALimit keeps the removal from being
// unconditional here too.
func TestTheVelocityJVMStillPreTouchesUnderALimit(t *testing.T) {
	out, err := runVelocityEntrypoint(t, t.TempDir(), 0, cgroupRoot(t, "2147483648", false))
	if err != nil {
		t.Fatalf("entrypoint: %v", err)
	}
	if !strings.Contains(javaArgv(t, out), "-XX:+AlwaysPreTouch") {
		t.Errorf("the flag was dropped under a limit; got: %s", out)
	}
}

func TestVelocityRefusesLangOnTheFileVolume(t *testing.T) {
	// lang/ belongs to Velocity itself: it migrates lang/messages.properties
	// to MiniMessage on every start and writes the result back, so a file
	// placed there is overwritten before anybody reads it. Nothing breaks,
	// which is exactly why it is refused -- a copy that silently does nothing
	// is worse than a collision that announces itself.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "lang"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "lang", "messages.properties"),
		[]byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

	if err == nil {
		t.Fatal("a source carrying lang/ started anyway")
	}
	if !strings.Contains(out, "lang/") {
		t.Errorf("the message does not name the directory:\n%s", out)
	}
}

func TestVelocityRefusesItsOwnRenderedFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "velocity.toml"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

	if err == nil {
		t.Fatal("a source carrying velocity.toml started anyway")
	}
	if !strings.Contains(out, "velocity.toml") {
		t.Errorf("the message does not name the file:\n%s", out)
	}
}

// TestVelocityRefusesPluginsOnTheFileVolume is the third entry design 5 lists
// for the proxy, alongside velocity.toml and lang/. The other two had tests
// and this one did not, which is how a proxy could have ended up quietly
// accepting a plugins/ tree that extraPlugins owns.
func TestVelocityRefusesPluginsOnTheFileVolume(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugins", "p.jar"), []byte("jar"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

	if err == nil {
		t.Fatal("a proxy source carrying plugins/ started anyway")
	}
	if !strings.Contains(out, "extraPlugins") {
		t.Errorf("the message does not name the mechanism that owns plugins/:\n%s", out)
	}
}

func TestVelocityDoesNotRefuseThePaperFiles(t *testing.T) {
	// The list follows the flavour. Nothing writes server.properties on a
	// proxy, so refusing it would be a rule with no reason behind it.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "server.properties"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("a proxy refused a file no proxy owns: %v", err)
	}
}

func TestVelocityCopiesAFileFromTheVolume(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "extra", "note.txt"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("the start failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "extra", "note.txt")); err != nil {
		t.Errorf("the file did not reach the working directory: %v", err)
	}
}
