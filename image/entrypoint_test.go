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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

// stubTools puts fake java and spawnery-config binaries on PATH, in place of
// the real ones. The entrypoint invokes both unqualified rather than by a
// hardcoded path — spawnery-config the same way it already invoked java —
// specifically so a double can stand in for either one here without writing
// anything outside the test's own temp directory. What Paper and Velocity
// actually read once spawnery-config runs for real is proven in
// internal/render and cmd/spawnery-config, not here; this package only owns
// the shell that wires the two real programs together.
//
// configExit is the exit code the spawnery-config stub returns, so the
// caller can simulate the renderer refusing without needing a real,
// unmounted /etc/spawnery to make it refuse on its own.
func stubTools(t *testing.T, configExit int) string {
	t.Helper()
	dir := t.TempDir()

	javaScript := "#!/bin/sh\nprintf 'JAVA_ARGV: %s\\n' \"$*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "java"), []byte(javaScript), 0o755); err != nil {
		t.Fatalf("write java stub: %v", err)
	}

	configScript := fmt.Sprintf("#!/bin/sh\nprintf 'SPAWNERY_CONFIG_ARGV: %%s\\n' \"$*\"\nexit %d\n", configExit)
	if err := os.WriteFile(filepath.Join(dir, "spawnery-config"), []byte(configScript), 0o755); err != nil {
		t.Fatalf("write spawnery-config stub: %v", err)
	}

	return dir
}

// runScript runs repoScript in workDir and returns its combined output.
// configExit controls whether the spawnery-config stub succeeds (0, what
// every test wants except the refusal one) or fails. Shared by
// image/entrypoint_test.go and image/velocity_entrypoint_test.go: both
// scripts invoke spawnery-config unqualified — deliberately, so a PATH stub
// can stand in for it — and this is that stub's one harness.
func runScript(t *testing.T, repoScript, workDir string, configExit int, env ...string) (string, error) {
	t.Helper()
	script := testenv.RepoPath(t, repoScript)

	cmd := exec.Command("sh", script)
	cmd.Dir = workDir
	cmd.Env = append([]string{
		"PATH=" + stubTools(t, configExit) + ":" + os.Getenv("PATH"),
	}, env...)

	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runEntrypoint runs the Paper entrypoint. PAPER_HOME defaults to /opt/paper,
// overridable through env the same way runScript passes any other variable.
func runEntrypoint(t *testing.T, workDir string, configExit int, env ...string) (string, error) {
	t.Helper()
	return runScript(t, "image/entrypoint.sh", workDir, configExit,
		append([]string{"PAPER_HOME=/opt/paper"}, env...)...)
}

func TestEntrypointAcceptsTheEula(t *testing.T) {
	dir := t.TempDir()

	if _, err := runEntrypoint(t, dir, 0); err != nil {
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

func TestEntrypointInvokesSpawneryConfigWithThePaperFlavor(t *testing.T) {
	dir := t.TempDir()

	// The one thing this script alone is responsible for getting right about
	// spawnery-config: passing --flavor paper rather than, say, copying
	// --flavor velocity from image/velocity-entrypoint.sh by accident. What
	// spawnery-config does with that flag — the files it writes, the layering
	// of ConfigMap, overlay and critical fields — is internal/render's and
	// cmd/spawnery-config's own coverage, not this package's.
	out, err := runEntrypoint(t, dir, 0)
	if err != nil {
		t.Fatalf("entrypoint: %v", err)
	}
	if !strings.Contains(out, "SPAWNERY_CONFIG_ARGV: --flavor paper") {
		t.Errorf("spawnery-config was not invoked with --flavor paper; got: %s", out)
	}
}

func TestEntrypointExecsJavaWithTheBundlerRepo(t *testing.T) {
	dir := t.TempDir()

	out, err := runEntrypoint(t, dir, 0)
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

func TestEntrypointStopsIfSpawneryConfigRefuses(t *testing.T) {
	dir := t.TempDir()

	// PAPER_HOME points at a real jar, so a script that pressed on regardless
	// of spawnery-config's exit code would still manage to copy the plugin
	// and start java — the two assertions below are what tell that apart from
	// a script that actually stopped.
	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paperHome, "agent", "spawnery-agent.jar"), []byte("fresh"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, dir, 1, "PAPER_HOME="+paperHome)
	if err == nil {
		t.Fatalf("entrypoint succeeded, want a failure; output: %s", out)
	}

	// It was actually reached and actually refused, not skipped by some
	// unrelated shell error earlier in the script.
	if !strings.Contains(out, "SPAWNERY_CONFIG_ARGV: --flavor paper") {
		t.Errorf("spawnery-config was never invoked; output: %s", out)
	}
	if strings.Contains(out, "JAVA_ARGV:") {
		t.Errorf("java was started anyway; output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "spawnery-agent.jar")); err == nil {
		t.Error("the agent plugin was copied anyway, after the renderer refused")
	}

	// The EULA write comes before spawnery-config on purpose: accepting
	// Mojang's EULA is not conditional on the renderer's opinion of the
	// operator's configuration, and a refusal here must not silently undo it
	// on a restart that later succeeds.
	if _, err := os.ReadFile(filepath.Join(dir, "eula.txt")); err != nil {
		t.Errorf("eula.txt was not written before the refusal: %v", err)
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

	if _, err := runEntrypoint(t, dir, 0, "PAPER_HOME="+paperHome); err != nil {
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

func TestCopiesTheAgentPluginOnASecondStartEvenThoughTheFirstLeftItReadOnly(t *testing.T) {
	dir := t.TempDir()

	// The jar ships read-only in the Nix store, and cp with no -p inherits the
	// source's mode — so the copy a first start leaves in plugins/ is 0444
	// too, the same state a real second start finds. Nothing in the
	// entrypoint chmods it: cp -f alone has to be able to replace it.
	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(paperHome, "agent", "spawnery-agent.jar")
	if err := os.WriteFile(jar, []byte("v1"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "PAPER_HOME="+paperHome); err != nil {
		t.Fatalf("first entrypoint run: %v", err)
	}

	copied := filepath.Join(dir, "plugins", "spawnery-agent.jar")
	info, err := os.Stat(copied)
	if err != nil {
		t.Fatalf("stat after first run: %v", err)
	}
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("setup invalid: the first run's copy is writable (mode %v); this test needs it read-only to prove the second run doesn't depend on a chmod", info.Mode().Perm())
	}

	// A new image ships a new jar. The source file must be removed before
	// rewriting it — it is 0444 itself, and os.WriteFile can't truncate a
	// read-only file it doesn't own the mode of.
	if err := os.Remove(jar); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jar, []byte("v2"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "PAPER_HOME="+paperHome); err != nil {
		t.Fatalf("second entrypoint run: %v", err)
	}

	got, err := os.ReadFile(copied)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("plugins/spawnery-agent.jar = %q after the second run, want %q — cp -f alone must replace a read-only leftover, with no chmod in between", got, "v2")
	}
}

// cgroupRoot writes a fake cgroup tree and returns the env var pointing the
// entrypoint at it. limit is the file's contents: "max" for cgroup v2's
// unbounded, a number for a real limit. v1 writes a different file, which the
// second argument selects.
func cgroupRoot(t *testing.T, limit string, v1 bool) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "memory.max")
	if v1 {
		if err := os.MkdirAll(filepath.Join(root, "memory"), 0o755); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(root, "memory", "memory.limit_in_bytes")
	}
	if err := os.WriteFile(path, []byte(limit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return "SPAWNERY_CGROUP_ROOT=" + root
}

// javaArgv is the line the stub java prints, and nothing else. Asserting
// against the whole output would be wrong in both directions here: the log
// line this check emits names AlwaysPreTouch in order to say it is not being
// used, so "the output contains the flag" is true exactly when the flag was
// dropped.
func javaArgv(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "JAVA_ARGV:") {
			return line
		}
	}
	t.Fatalf("java was never invoked; output: %s", out)
	return ""
}

// TestTheJVMDoesNotPreTouchWithoutAMemoryLimit is the hazard the flag creates
// outside a limit. AlwaysPreTouch claims the whole heap at start, and
// MaxRAMPercentage makes that heap a share of whatever bounds the container --
// the node, when nothing does. So one server with no resources.limits.memory
// took three quarters of the machine from every other pod on it, the instant
// it started.
func TestTheJVMDoesNotPreTouchWithoutAMemoryLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit string
		v1    bool
	}{
		{"cgroup v2 unbounded", "max", false},
		{"cgroup v1 sentinel", "9223372036854771712", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runEntrypoint(t, t.TempDir(), 0, cgroupRoot(t, tc.limit, tc.v1))
			if err != nil {
				t.Fatalf("entrypoint: %v", err)
			}
			argv := javaArgv(t, out)
			if strings.Contains(argv, "AlwaysPreTouch") {
				t.Errorf("the JVM pre-touches its heap with no memory limit; got: %s", argv)
			}
			// The rest of the tuning is untouched: this drops one flag, it
			// does not decide the JVM has no opinion about anything.
			if !strings.Contains(argv, "-XX:+UseG1GC") {
				t.Errorf("the other JVM flags went with it; got: %s", argv)
			}
			// And it says so, because a pod that quietly starts differently is
			// a pod nobody can diagnose.
			if !strings.Contains(out, "no memory limit") {
				t.Errorf("nothing in the log says why; got: %s", out)
			}
		})
	}
}

// TestTheJVMStillPreTouchesUnderALimit is the other half, and the one that
// keeps the check from being a silent removal of the flag everywhere. Inside a
// limit the trade AlwaysPreTouch makes is a good one and nothing changes.
func TestTheJVMStillPreTouchesUnderALimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit string
		v1    bool
	}{
		{"cgroup v2 with a limit", "2147483648", false},
		{"cgroup v1 with a limit", "2147483648", true},
		// An unreadable cgroup is treated as limited: the direction that
		// changes nothing. This is also what every developer machine running
		// make test lands on.
		{"no cgroup files at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := "SPAWNERY_CGROUP_ROOT=" + t.TempDir()
			if tc.limit != "" {
				env = cgroupRoot(t, tc.limit, tc.v1)
			}
			out, err := runEntrypoint(t, t.TempDir(), 0, env)
			if err != nil {
				t.Fatalf("entrypoint: %v", err)
			}
			if !strings.Contains(javaArgv(t, out), "-XX:+AlwaysPreTouch") {
				t.Errorf("the flag was dropped under a limit; got: %s", out)
			}
		})
	}
}

func TestPluginsFromTheVolumeAreCopiedInWithTheirConfiguration(t *testing.T) {
	dir := t.TempDir()

	// The volume, as the operator would have mounted it: a jar and a nested
	// configuration file. The configuration is half the point -- jars alone
	// would leave every plugin at its defaults on an ephemeral group, whose
	// /data is an emptyDir.
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "LuckPerms"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "luckperms.jar"), []byte("jar"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "LuckPerms", "config.yml"),
		[]byte("server: lobby"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_PLUGIN_SOURCE="+source); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	jar, err := os.ReadFile(filepath.Join(dir, "plugins", "luckperms.jar"))
	if err != nil {
		t.Fatalf("the jar did not reach the plugins directory: %v", err)
	}
	if string(jar) != "jar" {
		t.Errorf("plugins/luckperms.jar = %q, want the volume's copy", jar)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "plugins", "LuckPerms", "config.yml"))
	if err != nil {
		t.Fatalf("the nested configuration did not reach the plugins directory: %v", err)
	}
	if string(cfg) != "server: lobby" {
		t.Errorf("plugins/LuckPerms/config.yml = %q, want the volume's copy", cfg)
	}
}

func TestCopiedPluginFilesAreWritable(t *testing.T) {
	// The mount is read-only, so every file arrives read-only. Paper writes
	// its plugins' data folders inside this directory, and a plugin that
	// cannot rewrite its own config fails in its own way rather than in one
	// the server reports.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugin.jar"), []byte("jar"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_PLUGIN_SOURCE="+source); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "plugins", "plugin.jar"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Errorf("plugins/plugin.jar is %v, want it writable by its owner", info.Mode().Perm())
	}
}

func TestTheAgentJarWinsOverOneOnTheVolume(t *testing.T) {
	// The bound. Somebody pinning an older agent by dropping it on the volume
	// would otherwise leave the operator talking to a version it never
	// published -- and every object in the cluster would say the right thing.
	dir := t.TempDir()

	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paperHome, "agent", "spawnery-agent.jar"),
		[]byte("from the image"), 0o444); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "spawnery-agent.jar"),
		[]byte("from the volume"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0,
		"PAPER_HOME="+paperHome, "SPAWNERY_PLUGIN_SOURCE="+source); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "plugins", "spawnery-agent.jar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from the image" {
		t.Errorf("plugins/spawnery-agent.jar = %q, want the image's copy to win", got)
	}
}

func TestLostAndFoundIsSkippedRatherThanCopied(t *testing.T) {
	// Every ext4 filesystem has one, mode 0700 and owned by root, and Longhorn
	// formats ext4 by default -- so this is the ordinary case for a plugin
	// claim rather than an exotic one. A non-root container cannot read it,
	// and a copy that tried would fail the whole start under `set -eu`.
	//
	// Measured on a live claim before this guard existed: `cp -a` reported
	// "can't preserve ownership of '.../lost+found'" and exited 1.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugin.jar"), []byte("jar"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_PLUGIN_SOURCE="+source); err != nil {
		t.Fatalf("a source carrying lost+found failed the start: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "plugins", "lost+found")); err == nil {
		t.Error("lost+found was copied into the plugins directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "plugin.jar")); err != nil {
		t.Errorf("the real plugin did not survive the skip: %v", err)
	}
}

func TestADotfileOnTheVolumeIsCopiedToo(t *testing.T) {
	// The loop iterates two globs because a single `*` skips dotfiles, and a
	// plugin's data directory may well carry one. Without the second glob this
	// would silently drop them, which is the kind of gap nobody notices until
	// a plugin behaves oddly.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".keep"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_PLUGIN_SOURCE="+source); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "plugins", ".keep")); err != nil {
		t.Errorf("a dotfile on the volume was not copied: %v", err)
	}
}

func TestNoSourceDirectoryIsNotAnError(t *testing.T) {
	// The overwhelmingly common case: a group with no extraPlugins renders no
	// volume, so the path does not exist. Under `set -eu` a missing guard here
	// would fail every start in every installation.
	dir := t.TempDir()

	if _, err := runEntrypoint(t, dir, 0,
		"SPAWNERY_PLUGIN_SOURCE="+filepath.Join(dir, "nothing-here")); err != nil {
		t.Fatalf("a missing plugin source failed the start: %v", err)
	}
}
