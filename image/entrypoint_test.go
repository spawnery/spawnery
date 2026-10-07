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

// Package image tests the shell entrypoints of the base images.
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

// stubTools puts fake java and spawnery-config binaries on PATH. configExit
// is the exit code the spawnery-config stub returns.
func stubTools(t *testing.T, configExit int) string {
	t.Helper()
	dir := t.TempDir()

	javaScript := "#!/bin/sh\nprintf 'JAVA_ARGV: %s\\n' \"$*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "java"), []byte(javaScript), 0o755); err != nil {
		t.Fatalf("write java stub: %v", err)
	}

	configScript := fmt.Sprintf("#!/bin/sh\nprintf 'SPAWNERY_CONFIG_ARGV: %%s\\n' \"$*\"\n"+
		"if [ \"$1\" = --substitute ]; then exit \"${STUB_SUBSTITUTE_EXIT:-0}\"; fi\n"+
		"if [ \"$1\" = --prune ]; then exit \"${STUB_PRUNE_EXIT:-0}\"; fi\nexit %d\n", configExit)
	if err := os.WriteFile(filepath.Join(dir, "spawnery-config"), []byte(configScript), 0o755); err != nil {
		t.Fatalf("write spawnery-config stub: %v", err)
	}

	return dir
}

// runScript runs repoScript in workDir and returns its combined output. A
// PATH entry in env goes in front of the stubs rather than replacing them.
func runScript(t *testing.T, repoScript, workDir string, configExit int, env ...string) (string, error) {
	t.Helper()
	script := testenv.RepoPath(t, repoScript)

	path := stubTools(t, configExit) + ":" + os.Getenv("PATH")
	var rest []string
	for _, e := range env {
		if dirs, ok := strings.CutPrefix(e, "PATH="); ok {
			path = dirs + ":" + path
			continue
		}
		rest = append(rest, e)
	}

	cmd := exec.Command("sh", script)
	cmd.Dir = workDir
	cmd.Env = append([]string{"PATH=" + path}, rest...)

	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeLaunchFiles gives home the two files nix/flat-launch.nix writes into an
// image, and returns the class path it wrote.
func writeLaunchFiles(t *testing.T, home string) string {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	classpath := home + "/repo/versions/26.3/purpur-26.3.jar:" + home + "/repo/libraries/lib.jar"
	if err := os.WriteFile(filepath.Join(home, "launch.classpath"), []byte(classpath+"\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "launch.main"), []byte("org.bukkit.craftbukkit.Main"), 0o444); err != nil {
		t.Fatal(err)
	}
	return classpath
}

func runEntrypoint(t *testing.T, workDir string, configExit int, env ...string) (string, error) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "opt", "purpur")
	writeLaunchFiles(t, home)
	return runScript(t, "image/entrypoint.sh", workDir, configExit,
		append([]string{"SPAWNERY_PAPER_HOME=" + home}, env...)...)
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

	out, err := runEntrypoint(t, dir, 0)
	if err != nil {
		t.Fatalf("entrypoint: %v", err)
	}
	if !strings.Contains(out, "SPAWNERY_CONFIG_ARGV: --flavor paper") {
		t.Errorf("spawnery-config was not invoked with --flavor paper; got: %s", out)
	}
}

func TestEntrypointStartsTheServerFlatFromTheLaunchFiles(t *testing.T) {
	home := filepath.Join(t.TempDir(), "opt", "purpur")
	classpath := writeLaunchFiles(t, home)

	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_PAPER_HOME="+home)
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	argv := javaArgv(t, out)
	if !strings.HasSuffix(argv, " -cp "+classpath+" org.bukkit.craftbukkit.Main --nogui") {
		t.Errorf("java was not started flat from the launch files; got: %s", argv)
	}
	for _, unwanted := range []string{"-jar", "bundlerRepoDir"} {
		if strings.Contains(argv, unwanted) {
			t.Errorf("java still carries the bundler's %q; got: %s", unwanted, argv)
		}
	}
	if !strings.Contains(argv, "-XX:MaxRAMPercentage=75") {
		t.Errorf("the heap flag is gone; got: %s", argv)
	}
}

func TestEntrypointRefusesAnImageWithoutLaunchFiles(t *testing.T) {
	for _, missing := range []string{"launch.classpath", "launch.main"} {
		t.Run(missing, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "opt", "purpur")
			writeLaunchFiles(t, home)
			if err := os.Remove(filepath.Join(home, missing)); err != nil {
				t.Fatal(err)
			}

			out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_PAPER_HOME="+home)
			if err == nil {
				t.Fatalf("entrypoint succeeded without %s; output: %s", missing, out)
			}
			if !strings.Contains(out, home+"/"+missing+" is missing") {
				t.Errorf("the refusal does not name %s; output: %s", missing, out)
			}
			if strings.Contains(out, "JAVA_ARGV:") {
				t.Errorf("java was started anyway; output: %s", out)
			}
		})
	}
}

func TestEntrypointStopsIfSpawneryConfigRefuses(t *testing.T) {
	dir := t.TempDir()

	// SPAWNERY_PAPER_HOME points at a real jar, so only the assertions below tell
	// a script that stopped from one that pressed on.
	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLaunchFiles(t, paperHome)
	if err := os.WriteFile(filepath.Join(paperHome, "agent", "spawnery-agent.jar"), []byte("fresh"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, dir, 1, "SPAWNERY_PAPER_HOME="+paperHome)
	if err == nil {
		t.Fatalf("entrypoint succeeded, want a failure; output: %s", out)
	}

	// Reached and refused, not skipped by an earlier shell error.
	if !strings.Contains(out, "SPAWNERY_CONFIG_ARGV: --flavor paper") {
		t.Errorf("spawnery-config was never invoked; output: %s", out)
	}
	if strings.Contains(out, "JAVA_ARGV:") {
		t.Errorf("java was started anyway; output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "spawnery-agent.jar")); err == nil {
		t.Error("the agent plugin was copied anyway, after the renderer refused")
	}

	// The EULA is written before spawnery-config: accepting it does not depend
	// on the renderer, and a refusal must not undo it.
	if _, err := os.ReadFile(filepath.Join(dir, "eula.txt")); err != nil {
		t.Errorf("eula.txt was not written before the refusal: %v", err)
	}
}

func TestCopiesTheAgentPluginIntoAWritablePluginsDirectory(t *testing.T) {
	dir := t.TempDir()

	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLaunchFiles(t, paperHome)
	jar := filepath.Join(paperHome, "agent", "spawnery-agent.jar")
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

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_PAPER_HOME="+paperHome); err != nil {
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

	// cp keeps the store's 0444 mode, the state a real second start finds;
	// cp -f alone has to replace it.
	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLaunchFiles(t, paperHome)
	jar := filepath.Join(paperHome, "agent", "spawnery-agent.jar")
	if err := os.WriteFile(jar, []byte("v1"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_PAPER_HOME="+paperHome); err != nil {
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

	// The source is 0444 itself, and os.WriteFile cannot truncate it.
	if err := os.Remove(jar); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jar, []byte("v2"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_PAPER_HOME="+paperHome); err != nil {
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

// javaArgv is the line the stub java prints. The whole output would mislead:
// the log line names AlwaysPreTouch exactly when the flag is dropped.
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
			if !strings.Contains(argv, "-XX:+UseG1GC") {
				t.Errorf("the other JVM flags went with it; got: %s", argv)
			}
			if !strings.Contains(out, "no memory limit") {
				t.Errorf("nothing in the log says why; got: %s", out)
			}
		})
	}
}

func TestTheJVMStillPreTouchesUnderALimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit string
		v1    bool
	}{
		{"cgroup v2 with a limit", "2147483648", false},
		{"cgroup v1 with a limit", "2147483648", true},
		// An unreadable cgroup counts as limited; every developer machine lands here.
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
	dir := t.TempDir()

	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLaunchFiles(t, paperHome)
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
		"SPAWNERY_PAPER_HOME="+paperHome, "SPAWNERY_PLUGIN_SOURCE="+source); err != nil {
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
	// A single `*` skips dotfiles.
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
	// A group without extraPlugins has no volume: the common case.
	dir := t.TempDir()

	if _, err := runEntrypoint(t, dir, 0,
		"SPAWNERY_PLUGIN_SOURCE="+filepath.Join(dir, "nothing-here")); err != nil {
		t.Fatalf("a missing plugin source failed the start: %v", err)
	}
}

func TestAFileFromTheVolumeLandsUnderConfig(t *testing.T) {
	// The case the whole field exists for: a path no mount can reach, because
	// the kubelet would create /data/config root-owned.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "config", "sponge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config", "sponge", "sponge.conf"),
		[]byte("version=1\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("a source carrying config/sponge/sponge.conf failed the start: %v", err)
	}

	landed := filepath.Join(dir, "config", "sponge", "sponge.conf")
	info, err := os.Stat(landed)
	if err != nil {
		t.Fatalf("the file did not reach %s: %v", landed, err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Error("the copy is read-only; Sponge rewrites this file on every start")
	}
}

func TestAFileFromTheVolumeMergesIntoConfigInsteadOfNestingUnderIt(t *testing.T) {
	// The real spawnery-config creates config/ before the copy; the stub does
	// not, so this test creates it by hand.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "paper-global.yml"),
		[]byte("rendered-by-spawnery-config\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "config", "sponge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config", "sponge", "sponge.conf"),
		[]byte("version=1\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("a source carrying config/sponge/sponge.conf failed the start: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "config", "sponge", "sponge.conf")); err != nil {
		t.Errorf("the volume's file did not merge into the existing config directory: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "config", "paper-global.yml"))
	if err != nil {
		t.Fatalf("the renderer's file did not survive the merge: %v", err)
	}
	if string(got) != "rendered-by-spawnery-config\n" {
		t.Errorf("config/paper-global.yml = %q, want the renderer's file untouched", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "config")); err == nil {
		t.Error("the volume's config directory nested under config/config instead of merging into config/")
	}
}

func TestAFileTheRendererOwnsRefusesTheStart(t *testing.T) {
	for _, owned := range []string{
		"server.properties",
		"config/paper-global.yml",
		"config/paper-world-defaults.yml",
	} {
		t.Run(owned, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "volume")
			if err := os.MkdirAll(filepath.Join(source, filepath.Dir(owned)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, owned), []byte("x"), 0o444); err != nil {
				t.Fatal(err)
			}

			out, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

			if err == nil {
				t.Fatalf("a source carrying %s started anyway", owned)
			}
			if !strings.Contains(out, owned) {
				t.Errorf("the message does not name the file:\n%s", out)
			}
			if !strings.Contains(out, "extraFiles") {
				t.Errorf("the message does not name the field:\n%s", out)
			}
		})
	}
}

func TestAPluginOnTheFileVolumeRefusesTheStart(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugins", "p.jar"), []byte("jar"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

	if err == nil {
		t.Fatal("a source carrying plugins/ started anyway")
	}
	if !strings.Contains(out, "extraPlugins") {
		t.Errorf("the message does not name the mechanism that owns plugins/:\n%s", out)
	}
}

func TestARegularFileNamedPluginsRefusesTheStart(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugins"), []byte("not a directory"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

	if err == nil {
		t.Fatal("a source carrying a regular file named plugins started anyway")
	}
	if !strings.Contains(out, "extraPlugins") {
		t.Errorf("the message does not name the mechanism that owns plugins/:\n%s", out)
	}
}

func TestAClaimCarryingTheEULARefusesTheStart(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "eula.txt"), []byte("eula=false\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

	if err == nil {
		t.Fatal("a source carrying eula.txt started anyway")
	}
	if !strings.Contains(out, "eula.txt") {
		t.Errorf("the message does not name the file:\n%s", out)
	}
}

// Nothing on a Paper server writes velocity.toml or lang/, so refusing them
// would crash-loop a group for no reason.
func TestPaperDoesNotRefuseTheVelocityFiles(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "lang"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "lang", "messages.properties"),
		[]byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "velocity.toml"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("a Paper server refused a file no Paper server owns: %v", err)
	}

	for _, want := range []string{"velocity.toml", filepath.Join("lang", "messages.properties")} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s did not reach the working directory: %v", want, err)
		}
	}
}

func TestNothingIsCopiedWhenTheScanRefuses(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "config", "sponge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config", "sponge", "sponge.conf"),
		[]byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "server.properties"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err == nil {
		t.Fatal("the start was not refused")
	}

	if _, err := os.Stat(filepath.Join(dir, "config", "sponge", "sponge.conf")); err == nil {
		t.Error("the good file was copied before the bad one was noticed")
	}
}

func TestLostFoundOnTheFileVolumeIsSkipped(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "bukkit.yml"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("a source carrying lost+found failed the start: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "lost+found")); err == nil {
		t.Error("lost+found was copied into the working directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "bukkit.yml")); err != nil {
		t.Errorf("the real file did not survive the skip: %v", err)
	}
}

func TestNoFileVolumeIsNotAnError(t *testing.T) {
	dir := t.TempDir()

	if _, err := runEntrypoint(t, dir, 0,
		"SPAWNERY_FILE_SOURCE="+filepath.Join(dir, "nothing-here")); err != nil {
		t.Fatalf("a start with no file volume failed: %v", err)
	}
}

func TestSubstituteRunsOnlyWithAPrefix(t *testing.T) {
	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_SUBSTITUTION_PREFIX=SECRET_")
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if !strings.Contains(out, "SPAWNERY_CONFIG_ARGV: --substitute SECRET_ --pair ") {
		t.Errorf("no substitute call:\n%s", out)
	}
	if strings.Index(out, "--substitute") > strings.Index(out, "JAVA_ARGV") {
		t.Error("substitution ran after the JVM started")
	}
	out, err = runEntrypoint(t, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if strings.Contains(out, "--substitute") {
		t.Errorf("substitute ran without a prefix:\n%s", out)
	}
}

func TestEntrypointStopsIfSubstitutionRefuses(t *testing.T) {
	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_SUBSTITUTION_PREFIX=SECRET_", "STUB_SUBSTITUTE_EXIT=1")
	if err == nil || strings.Contains(out, "JAVA_ARGV") {
		t.Errorf("the JVM started after a refusal:\n%s", out)
	}
}

func TestPruneRunsOnlyWithKeepEntriesAndBeforeTheRenderer(t *testing.T) {
	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_KEEP=world\nplugins/ExampleGame/state")
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if !strings.Contains(out, "SPAWNERY_CONFIG_ARGV: --prune world\nplugins/ExampleGame/state --mountinfo ") {
		t.Errorf("no prune call:\n%s", out)
	}
	if strings.Index(out, "--prune") > strings.Index(out, "--flavor paper") {
		t.Error("prune ran after the renderer")
	}
	out, err = runEntrypoint(t, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if strings.Contains(out, "--prune") {
		t.Errorf("prune ran without keep entries:\n%s", out)
	}
}

func TestPruneRunsBeforeTheEulaIsWritten(t *testing.T) {
	dir := t.TempDir()
	out, err := runEntrypoint(t, dir, 0, "SPAWNERY_KEEP=world", "STUB_PRUNE_EXIT=1")
	if err == nil {
		t.Fatalf("entrypoint succeeded after a refusing prune:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "eula.txt")); err == nil {
		t.Error("eula.txt was written before prune ran")
	}
}

// The prune exempts what a source ships, so a source refused after it would
// have its shipped worlds deleted and never copied back.
func TestARefusedSourceNeverReachesThePrune(t *testing.T) {
	tests := map[string]struct {
		files, plugins []string
		mounts         []string
	}{
		"extraFiles carrying plugins/":         {files: []string{"plugins/Map/level.dat"}},
		"extraFiles carrying eula.txt":         {files: []string{"eula.txt"}},
		"extraFiles carrying a rendered file":  {files: []string{"server.properties"}},
		"extraFiles under a read-only mount":   {files: []string{"mods/pack.jar"}, mounts: []string{"mods"}},
		"extraPlugins under a read-only mount": {plugins: []string{"Shop/data.yml"}, mounts: []string{"plugins/Shop"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, "plugins/Map/level.dat")
			files := filepath.Join(t.TempDir(), "files")
			plugins := filepath.Join(t.TempDir(), "plugins")
			writeTree(t, files, tc.files...)
			writeTree(t, plugins, tc.plugins...)

			out, err := runEntrypoint(t, dir, 0,
				"SPAWNERY_KEEP=world",
				"SPAWNERY_FILE_SOURCE="+files,
				"SPAWNERY_PLUGIN_SOURCE="+plugins,
				"SPAWNERY_MOUNTINFO="+fakeMountinfo(t, dir, tc.mounts...))

			if err == nil {
				t.Fatalf("the start was not refused:\n%s", out)
			}
			if !strings.Contains(out, "Refusing to start") {
				t.Errorf("not refused by the source scan:\n%s", out)
			}
			if strings.Contains(out, "--prune") {
				t.Errorf("the prune ran before the refusal:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(dir, "plugins", "Map", "level.dat")); err != nil {
				t.Errorf("the claim changed: %v", err)
			}
		})
	}
}

func TestEntrypointStopsIfPruneRefuses(t *testing.T) {
	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_KEEP=world", "STUB_PRUNE_EXIT=1")
	if err == nil || strings.Contains(out, "JAVA_ARGV") || strings.Contains(out, "--flavor paper") {
		t.Errorf("the start went on after a refusal:\n%s", out)
	}
}

func TestPrunePassesTheReplaceEntries(t *testing.T) {
	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_KEEP=world", "SPAWNERY_REPLACE=worlds/templates\nworlds/arena")
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--replace worlds/templates\nworlds/arena") {
		t.Errorf("replace entries did not reach the prune:\n%s", out)
	}
	out, err = runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_REPLACE=worlds/templates")
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if strings.Contains(out, "--prune") {
		t.Errorf("prune ran with replace entries but no keep entries:\n%s", out)
	}
}

func TestEntrypointUsesAMountedAOTCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "server.aot")
	if err := os.WriteFile(cache, []byte("cache"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_AOT_CACHE="+cache)
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	argv := javaArgv(t, out)
	flag := "-XX:AOTCache=" + cache
	at, cp := strings.Index(argv, flag), strings.Index(argv, " -cp ")
	if at < 0 || cp < 0 || at > cp {
		t.Errorf("want %s among the JVM options, before -cp; got: %s", flag, argv)
	}
}

// Adapters in the cache are machine code for the CPU the cache was trained
// on; on a node whose CPU lacks one of its instructions the JVM dies with
// SIGILL instead of dropping them.
func TestEntrypointLoadsNoAdaptersFromTheAOTCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "server.aot")
	if err := os.WriteFile(cache, []byte("cache"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_AOT_CACHE="+cache)
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	argv := javaArgv(t, out)
	unlock, off := strings.Index(argv, "-XX:+UnlockDiagnosticVMOptions"), strings.Index(argv, "-XX:-AOTAdapterCaching")
	cp := strings.Index(argv, " -cp ")
	if unlock < 0 || off < 0 || unlock > off || off > cp {
		t.Errorf("want -XX:+UnlockDiagnosticVMOptions -XX:-AOTAdapterCaching before -cp next to the cache; got: %s", argv)
	}

	out, err = runEntrypoint(t, t.TempDir(), 0,
		"SPAWNERY_AOT_CACHE="+filepath.Join(t.TempDir(), "server.aot"))
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if argv := javaArgv(t, out); strings.Contains(argv, "AOTAdapterCaching") {
		t.Errorf("an adapter flag without a cache file; got: %s", argv)
	}
}

func TestEntrypointStartsWithoutACacheWhenNoneIsMounted(t *testing.T) {
	out, err := runEntrypoint(t, t.TempDir(), 0,
		"SPAWNERY_AOT_CACHE="+filepath.Join(t.TempDir(), "server.aot"))
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if argv := javaArgv(t, out); strings.Contains(argv, "AOTCache") {
		t.Errorf("an AOT flag without a cache file; got: %s", argv)
	}
}

func TestEntrypointTrainsInsteadOfUsingACacheWhenAskedTo(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "server.aot")
	if err := os.WriteFile(cache, []byte("cache"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, t.TempDir(), 0,
		"SPAWNERY_AOT_CACHE="+cache, "SPAWNERY_AOT_OUTPUT=/aot/server.aot",
		cgroupRoot(t, "4294967296", false))
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	argv := javaArgv(t, out)
	if !strings.Contains(argv, "-XX:AOTCacheOutput=/aot/server.aot") {
		t.Errorf("no training flag; got: %s", argv)
	}
	if strings.Contains(argv, "-XX:AOTCache=") {
		t.Errorf("a training run also maps the old cache; got: %s", argv)
	}
	if strings.Contains(argv, "AlwaysPreTouch") {
		t.Errorf("a training run pre-touches, and the cache's child JVM would be OOM-killed; got: %s", argv)
	}
}

// The JVM refuses to start, rather than drop the cache, when -XX:AOTCache
// meets one of the CDS options a group can pass through the JVM's own variables.
func TestEntrypointLeavesTheCacheOutForAGroupsCDSOptions(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "server.aot")
	if err := os.WriteFile(cache, []byte("cache"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{
		"JAVA_TOOL_OPTIONS=-Xshare:off",
		"JAVA_TOOL_OPTIONS=-Xlog:gc -Xshare:auto",
		"JAVA_TOOL_OPTIONS=-XX:SharedArchiveFile=/data/app.jsa",
		"JDK_JAVA_OPTIONS=-XX:SharedClassListFile=/data/classes.lst",
		"JDK_JAVA_OPTIONS=-XX:DumpLoadedClassList=/data/classes.lst",
	} {
		t.Run(env, func(t *testing.T) {
			out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_AOT_CACHE="+cache, env)
			if err != nil {
				t.Fatalf("entrypoint: %v\n%s", err, out)
			}
			if argv := javaArgv(t, out); strings.Contains(argv, "AOTCache") {
				t.Errorf("the cache flag next to %s stops the JVM; got: %s", env, argv)
			}
			if !strings.Contains(out, "starting without the startup cache") {
				t.Errorf("nothing says why the cache was left out; output: %s", out)
			}
		})
	}
}

func TestEntrypointKeepsTheCacheForOtherJVMOptions(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "server.aot")
	if err := os.WriteFile(cache, []byte("cache"), 0o444); err != nil {
		t.Fatal(err)
	}
	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_AOT_CACHE="+cache,
		"JAVA_TOOL_OPTIONS=-Xlog:gc -Dfoo=bar")
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if argv := javaArgv(t, out); !strings.Contains(argv, "-XX:AOTCache="+cache) {
		t.Errorf("the cache was dropped for options that do not touch it; got: %s", argv)
	}
}

// chmodRefusing puts a chmod on PATH that fails the way chmod fails on a file
// owned by another user, for any path containing marker, and returns its
// directory.
func chmodRefusing(t *testing.T, marker string) string {
	t.Helper()
	real, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
for a; do
	case "$a" in
	*%s*) echo "chmod: changing permissions of '$a': Operation not permitted" >&2; exit 1 ;;
	esac
done
exec %s "$@"
`, marker, real)
	if err := os.WriteFile(filepath.Join(dir, "chmod"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTheFileCopyLeavesAWorldItDidNotCopyAlone(t *testing.T) {
	// The world sync node agent writes the world before the container starts,
	// as root with the pod's fsGroup: writable for the server, but not its own
	// to chmod.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "worlds", "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worlds", "world", "level.dat"), []byte("level"), 0o664); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "worlds", "world_templates", "lobby"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "worlds", "world_templates", "lobby", "level.dat"),
		[]byte("lobby"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source,
		"PATH="+chmodRefusing(t, "/world/"))
	if err != nil {
		t.Fatalf("a world beside the copied templates failed the start: %v\n%s", err, out)
	}

	info, err := os.Stat(filepath.Join(dir, "worlds", "world_templates", "lobby", "level.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Errorf("the copied template is %v, want it writable by its owner", info.Mode().Perm())
	}
}

func TestThePluginCopyLeavesPluginDataItDidNotCopyAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "plugins", "Game", "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugins", "Game", "internal", "state.json"), []byte("{}"), 0o664); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "Game"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "Game", "config.yml"), []byte("a: b"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, dir, 0, "SPAWNERY_PLUGIN_SOURCE="+source,
		"PATH="+chmodRefusing(t, "/internal"))
	if err != nil {
		t.Fatalf("plugin data beside the copied plugin failed the start: %v\n%s", err, out)
	}

	info, err := os.Stat(filepath.Join(dir, "plugins", "Game", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Errorf("the copied config is %v, want it writable by its owner", info.Mode().Perm())
	}
}
