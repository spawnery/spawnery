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

package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runVelocityEntrypoint(t *testing.T, workDir string, configExit int, env ...string) (string, error) {
	t.Helper()
	return runScript(t, "image/velocity-entrypoint.sh", workDir, configExit,
		append([]string{"SPAWNERY_VELOCITY_HOME=/opt/velocity"}, env...)...)
}

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

// A proxy started without valid forwarding configuration fails every join on
// the network while looking healthy, so the JVM must not start.
func TestVelocityEntrypointStopsIfSpawneryConfigRefuses(t *testing.T) {
	dir := t.TempDir()

	velocityHome := filepath.Join(dir, "opt", "velocity")
	if err := os.MkdirAll(filepath.Join(velocityHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(velocityHome, "agent", "spawnery-agent.jar"), []byte("fresh"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runVelocityEntrypoint(t, dir, 1, "SPAWNERY_VELOCITY_HOME="+velocityHome)
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

	if _, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_VELOCITY_HOME="+velocityHome); err != nil {
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

func TestVelocitySubstituteRunsOnlyWithAPrefix(t *testing.T) {
	out, err := runVelocityEntrypoint(t, t.TempDir(), 0, "SPAWNERY_SUBSTITUTION_PREFIX=SECRET_")
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if !strings.Contains(out, "SPAWNERY_CONFIG_ARGV: --substitute SECRET_ --pair ") ||
		strings.Index(out, "--substitute") > strings.Index(out, "JAVA_ARGV") {
		t.Errorf("no substitute call before the JVM:\n%s", out)
	}
	out, err = runVelocityEntrypoint(t, t.TempDir(), 0)
	if err != nil || strings.Contains(out, "--substitute") {
		t.Errorf("substitute ran without a prefix (err %v):\n%s", err, out)
	}
}

func TestVelocityEntrypointStopsIfSubstitutionRefuses(t *testing.T) {
	out, err := runVelocityEntrypoint(t, t.TempDir(), 0, "SPAWNERY_SUBSTITUTION_PREFIX=SECRET_", "STUB_SUBSTITUTE_EXIT=1")
	if err == nil || strings.Contains(out, "JAVA_ARGV") {
		t.Errorf("the JVM started after a refusal:\n%s", out)
	}
}
