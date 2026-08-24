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
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func paperValues() Values {
	n := int32(100)
	return Values{MaxPlayers: &n}
}

// The two file names Paper reads, at the paths it reads them from.
func TestPaperWritesBothFiles(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	for _, name := range []string{"server.properties", "config/paper-global.yml"} {
		if _, ok := files[name]; !ok {
			t.Errorf("no %s among %v", name, keysOf(files))
		}
	}
}

// The inversion this milestone exists to get right, on the backend half.
func TestPaperTurnsOnlineModeOff(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	props := string(files["server.properties"])
	if !strings.Contains(props, "online-mode=false") {
		t.Errorf("server.properties does not contain online-mode=false:\n%s", props)
	}
}

// Paper uses the same two words for the opposite setting: paper-global.yml's
// proxies.velocity.online-mode means "trust what Velocity forwards", and it
// must be true while server.properties says false. Both at once is correct.
func TestPaperEnablesVelocityForwarding(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	global := string(files["config/paper-global.yml"])
	for _, want := range []string{"enabled: true", "online-mode: true", "secret: s3cret"} {
		if !strings.Contains(global, want) {
			t.Errorf("paper-global.yml does not contain %q:\n%s", want, global)
		}
	}
}

// paperGlobalDefault is Paper's own config/paper-global.yml, byte for byte as
// the pinned build writes it on a first start against an otherwise empty data
// directory. It is a measurement of the receiving program, not of this
// package, which is the whole reason it exists — every other test in this file
// asserts that the renderer writes the string the renderer says it writes, and
// no such test can fail on a key Paper does not read. Reproduce it with:
//
//	REPO=$(nix build .#paper-repo --no-link --print-out-paths)
//	JAR=$(nix build .#paper-jar --no-link --print-out-paths)
//	JAVA=$(nix build nixpkgs#jdk25_headless --no-link --print-out-paths)/bin/java
//	mkdir /tmp/paper-defaults && cd /tmp/paper-defaults && echo eula=true >eula.txt
//	"$JAVA" -Xmx1g -DbundlerRepoDir="$REPO" -jar "$JAR" --nogui
//	# wait for config/paper-global.yml to appear, then stop the server
//	cp config/paper-global.yml "$OLDPWD"/internal/render/defaults/paper-global.default.yml
//
// (The dev shell's JDK is older than the 25 Paper 26.2 refuses to start
// without, hence the separate jdk25_headless — the same one nix/paper.nix
// patches the repo with. -Xmx1g only keeps the measurement off the host's
// whole memory; it has no bearing on the file.)
//
// A Paper bump therefore has to re-run this and update the file, exactly the
// way nix/velocity.nix's config-version carries the jar xf command that
// measured it. It is also not the only guard: hack/image-test.sh boots the
// pinned Paper against a rendered file and reads back what Paper made of it,
// which needs no fixture to be refreshed and fails on a rename by itself.
const paperGlobalDefault = defaultsDir + "/paper-global.default.yml"

// The keys this renderer writes have to be keys Paper declares.
//
// Paper does not refuse a key it does not know. It ignores it, keeps its own
// default for the field the author meant, and writes the stray key back out on
// the next save so the file on disk still looks like the override took.
// Measured against the pinned build: rendering secret-key rather than secret
// leaves Paper with an empty secret and, through its own postProcess, enabled:
// false — a backend that starts clean, passes every probe, and rejects every
// forwarded join with "Your server did not send a forwarding request to the
// proxy" at the other end. That was true of this renderer from the day it was
// written until milestone 3c's first end-to-end join.
func TestPaperWritesTheKeysPaperItselfReads(t *testing.T) {
	defaults, err := os.ReadFile(paperGlobalDefault)
	if err != nil {
		t.Fatalf("read Paper's own defaults: %v", err)
	}
	paperKeys := velocityKeysOf(t, defaults, paperGlobalDefault)

	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	rendered := files["config/paper-global.yml"]

	for _, key := range sortedKeys(velocityKeysOf(t, rendered, "the rendered config/paper-global.yml")) {
		if !paperKeys[key] {
			t.Errorf("the renderer writes proxies.velocity.%s, which Paper does not declare; Paper reads %v and would silently keep its own defaults for whatever this key was meant to set:\n%s",
				key, sortedKeys(paperKeys), rendered)
		}
	}
}

// velocityKeysOf reads the proxies.velocity key names out of a paper-global.yml
// document. It fails rather than returning an empty set when the block is
// missing, so a fixture that was truncated or a renderer that stopped writing
// the block at all cannot pass by having nothing to compare.
func velocityKeysOf(t *testing.T, doc []byte, what string) map[string]bool {
	t.Helper()
	var parsed struct {
		Proxies struct {
			Velocity map[string]any `json:"velocity"`
		} `json:"proxies"`
	}
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("%s does not parse as YAML: %v", what, err)
	}
	if len(parsed.Proxies.Velocity) == 0 {
		t.Fatalf("%s has no proxies.velocity mapping:\n%s", what, doc)
	}
	keys := make(map[string]bool, len(parsed.Proxies.Velocity))
	for k := range parsed.Proxies.Velocity {
		keys[k] = true
	}
	return keys
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestPaperCarriesMaxPlayersThrough(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	if !strings.Contains(string(files["server.properties"]), "max-players=100") {
		t.Error("max-players did not reach server.properties")
	}
}

// An overlay reaches a field the API does not model.
func TestPaperOverlayReachesAnUnmodelledField(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"server.properties": "view-distance=8\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	if !strings.Contains(string(files["server.properties"]), "view-distance=8") {
		t.Error("the overlay did not reach server.properties")
	}
}

// And cannot reach a critical one.
func TestPaperOverlayCannotTurnOnlineModeOn(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"server.properties": "online-mode=true\nserver-port=1234\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	props := string(files["server.properties"])
	if strings.Contains(props, "online-mode=true") {
		t.Errorf("an overlay turned online-mode on:\n%s", props)
	}
	if !strings.Contains(props, "server-port=25565") {
		t.Errorf("an overlay moved the port:\n%s", props)
	}
}

func TestPaperRefusesAnEmptySecret(t *testing.T) {
	_, err := Paper(paperValues(), "", nil)
	if err == nil {
		t.Fatal("an empty forwarding secret was accepted")
	}
	if !strings.Contains(err.Error(), "forwarding secret") {
		t.Errorf("error = %q, want it to name the secret", err)
	}
}

func TestPaperRefusesAnOverlayForAFileItDoesNotWrite(t *testing.T) {
	_, err := Paper(paperValues(), "s3cret", map[string]string{"velocity.toml": "x = 1\n"})
	if err == nil {
		t.Fatal("an overlay for a foreign file was accepted")
	}
	if !strings.Contains(err.Error(), "velocity.toml") {
		t.Errorf("error = %q, want it to name the file", err)
	}
}

// The paper-global.yml analogue of TestPaperOverlayCannotTurnOnlineModeOn: an
// overlay that tries to move all three of the critical Velocity keys at once
// must lose on every one of them. This is the failure this task exists to
// prevent — swap paperGlobal's copy-then-reassert order by one line and a
// backend trusts nothing Velocity forwards, handing every player an
// offline-mode UUID and detaching them from their own inventories — and
// nothing before this test would have caught that swap.
func TestPaperOverlayCannotMoveVelocityCriticalKeys(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "proxies:\n  velocity:\n    enabled: false\n    online-mode: false\n    secret: not-the-real-secret\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	global := string(files["config/paper-global.yml"])
	for _, want := range []string{"enabled: true", "online-mode: true", "secret: s3cret"} {
		if !strings.Contains(global, want) {
			t.Errorf("paper-global.yml does not contain %q, the overlay moved a critical key:\n%s", want, global)
		}
	}
	for _, unwanted := range []string{"enabled: false", "online-mode: false", "not-the-real-secret"} {
		if strings.Contains(global, unwanted) {
			t.Errorf("paper-global.yml contains %q, an overlay value that should have been clobbered:\n%s", unwanted, global)
		}
	}
}

// The key that cost milestone 3c its first end-to-end join, arriving the other
// way round. render.Paper wrote proxies.velocity.secret-key for a reader that
// wanted secret; that spelling is fixed and pinned, and an overlay was the one
// remaining path by which a second one could still have got in.
//
// Paper declares exactly three keys in that block and this operator writes all
// three, so nothing an overlay puts there could ever have applied: it is either
// overwritten here or ignored by Paper. Refusing says so at render time, where
// the alternative said nothing at all in a cluster.
func TestPaperRefusesAVelocityKeyPaperDoesNotDeclare(t *testing.T) {
	_, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "proxies:\n  velocity:\n    secret-key: s3cret\n",
	})
	if err == nil {
		t.Fatal("an overlay setting proxies.velocity.secret-key was accepted")
	}
	if !strings.Contains(err.Error(), "secret-key") {
		t.Errorf("error = %q, want it to name the key", err)
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Errorf("error = %q, want it to name what Paper does declare there", err)
	}
}

// A proxies key that is not a mapping is not a harmless overlay: it is a
// mistake that would otherwise silently do nothing while the server comes up
// looking healthy. The renderer must say so rather than swallow it, the same
// way it refuses a missing forwarding secret.
//
// The assertion checks for "want a mapping" rather than just "paper-global.yml":
// checkOverlayFiles' foreign-file rejection also names the file, so a
// looser assertion would still pass if the paperOverlayKeys fix that makes
// this overlay reach paperGlobal at all were reverted — it would just be
// rejected for the wrong reason.
func TestPaperRefusesAMalformedOverlay(t *testing.T) {
	_, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "proxies: not-a-map\n",
	})
	if err == nil {
		t.Fatal("a malformed paper-global.yml overlay was accepted")
	}
	if !strings.Contains(err.Error(), "paper-global.yml") {
		t.Errorf("error = %q, want it to name the file", err)
	}
	if !strings.Contains(err.Error(), "want a mapping") {
		t.Errorf("error = %q, want it to name the shape problem, not just the file", err)
	}
}

// The same refusal one level deeper: proxies is a mapping but its velocity
// key is not. Nothing before this test exercised this branch at all.
func TestPaperRefusesAMalformedVelocityOverlay(t *testing.T) {
	_, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "proxies:\n  velocity: not-a-map\n",
	})
	if err == nil {
		t.Fatal("a malformed proxies.velocity overlay was accepted")
	}
	if !strings.Contains(err.Error(), "proxies.velocity") {
		t.Errorf("error = %q, want it to name proxies.velocity", err)
	}
	if !strings.Contains(err.Error(), "want a mapping") {
		t.Errorf("error = %q, want it to name the shape problem, not just the key", err)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// An overlay key outside proxies.velocity has to reach the rendered file, and
// until this test nothing looked. paperGlobal built the document from scratch
// as {proxies: {velocity: ...}} and read the overlay only for its Velocity
// keys, so every other part of paper-global.yml an overlay set was parsed and
// dropped -- while paperOverlayKeys advertises the file as one an overlay may
// set, and checkOverlayFiles' own comment calls a silently dropped overlay key
// worse than an error. The two keys below are Paper's, chosen from opposite
// ends of its document so that neither could pass by sitting near the block
// the renderer already writes.
func TestPaperOverlayReachesTheRestOfPaperGlobal(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "misc:\n  max-joins-per-tick: 5\nwatchdog:\n  early-warning-delay: 12000\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	global := string(files["config/paper-global.yml"])
	for _, want := range []string{"max-joins-per-tick: 5", "early-warning-delay: 12000"} {
		if !strings.Contains(global, want) {
			t.Errorf("paper-global.yml does not contain %q; an overlay outside proxies.velocity "+
				"was accepted and then dropped, which is the failure checkOverlayFiles refuses "+
				"one level up:\n%s", want, global)
		}
	}
	// And the Velocity block is still asserted over whatever the overlay
	// carried, which is the half that must not be traded away for the half
	// above.
	for _, want := range []string{"enabled: true", "online-mode: true", "secret: s3cret"} {
		if !strings.Contains(global, want) {
			t.Errorf("paper-global.yml does not contain %q, the critical Velocity block did not "+
				"survive an overlay that carries the rest of the document:\n%s", want, global)
		}
	}
}
