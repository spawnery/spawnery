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
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func paperValues() Values {
	n := int32(100)
	return Values{MaxPlayers: &n}
}

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

// paper-global.yml's proxies.velocity.online-mode means "trust what Velocity
// forwards" and must be true while server.properties says false.
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

// paperGlobalDefault is Paper's own config/paper-global.yml as the pinned build
// writes it on a first start. A Paper bump has to regenerate it with:
//
//	REPO=$(nix build .#paper-repo --no-link --print-out-paths)
//	JAR=$(nix build .#paper-jar --no-link --print-out-paths)
//	JAVA=$(nix build nixpkgs#jdk25_headless --no-link --print-out-paths)/bin/java
//	mkdir /tmp/paper-defaults && cd /tmp/paper-defaults && echo eula=true >eula.txt
//	"$JAVA" -Xmx1g -DbundlerRepoDir="$REPO" -jar "$JAR" --nogui
//	# wait for config/paper-global.yml to appear, then stop the server
//	cp config/paper-global.yml "$OLDPWD"/internal/render/defaults/paper-global.default.yml
//
// jdk25_headless because the dev shell's JDK is older than Paper requires.
const paperGlobalDefault = defaultsDir + "/paper-global.default.yml"

// Paper ignores an undeclared key and writes it back out, so only a
// comparison against Paper's own defaults catches a misspelling such as
// secret-key, which leaves Velocity forwarding disabled.
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

// velocityKeysOf fails on a missing block, so a truncated fixture cannot pass
// by having nothing to compare.
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

func TestPaperTurnsTheWhitelistOffUnlessAskedFor(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	if props := string(files["server.properties"]); !strings.Contains(props, "white-list=false") {
		t.Errorf("server.properties does not contain white-list=false:\n%s", props)
	}

	files, err = Paper(paperValues(), "s3cret", map[string]string{
		"server.properties": "white-list=true\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	if props := string(files["server.properties"]); !strings.Contains(props, "white-list=true") {
		t.Errorf("the overlay did not turn the whitelist on:\n%s", props)
	}
}

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

// Swapping paperGlobal's copy-then-reassert order would hand every player an
// offline-mode UUID.
func TestPaperOverlayCannotMoveVelocityCriticalKeys(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "proxies:\n  velocity:\n    enabled: false\n    online-mode: false\n    secret: not-the-real-secret\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	// Read out of the document: the renderer writes other `enabled` keys too.
	global := string(files["config/paper-global.yml"])
	doc := map[string]any{}
	if err := yaml.Unmarshal(files["config/paper-global.yml"], &doc); err != nil {
		t.Fatalf("paper-global.yml does not parse: %v\n%s", err, global)
	}
	proxies, _ := doc["proxies"].(map[string]any)
	velocity, _ := proxies["velocity"].(map[string]any)
	if velocity == nil {
		t.Fatalf("paper-global.yml has no proxies.velocity block:\n%s", global)
	}
	for key, want := range map[string]any{"enabled": true, "online-mode": true, "secret": "s3cret"} {
		if got := velocity[key]; got != want {
			t.Errorf("proxies.velocity.%s = %v, want %v -- the overlay moved a critical key:\n%s",
				key, got, want, global)
		}
	}
	if strings.Contains(global, "not-the-real-secret") {
		t.Errorf("paper-global.yml still carries the overlay's secret:\n%s", global)
	}
}

// Paper declares exactly three keys in that block and the operator writes all
// three, so an overlay key there could never apply.
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

// Checks for "want a mapping", since checkOverlayFiles' rejection also names
// the file and would pass for the wrong reason.
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

// Two of Paper's keys from opposite ends of its document, so neither passes
// by sitting near the Velocity block.
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
	for _, want := range []string{"enabled: true", "online-mode: true", "secret: s3cret"} {
		if !strings.Contains(global, want) {
			t.Errorf("paper-global.yml does not contain %q, the critical Velocity block did not "+
				"survive an overlay that carries the rest of the document:\n%s", want, global)
		}
	}
}

func TestPaperTurnsOffTheUpdateChecker(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	// A map rather than a tagged struct: sigs.k8s.io/yaml reads `json` tags, so a
	// `yaml` tag would leave every field nil.
	if got := updateCheckerEnabled(t, files); got != false {
		t.Errorf("update-checker.enabled = %v, want false", got)
	}
}

// A default, not a critical key: unlike the Velocity block it decides nothing
// about joins, so the user wins.
func TestAnOverlayCanTurnTheUpdateCheckerBackOn(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "update-checker:\n  enabled: true\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	if got := updateCheckerEnabled(t, files); got != true {
		t.Errorf("update-checker.enabled = %v, want the overlay's true", got)
	}
}

// updateCheckerEnabled fails on a missing key, which would otherwise read as
// "off".
func updateCheckerEnabled(t *testing.T, files map[string][]byte) bool {
	t.Helper()
	doc := map[string]any{}
	if err := yaml.Unmarshal(files["config/paper-global.yml"], &doc); err != nil {
		t.Fatalf("paper-global.yml does not parse: %v", err)
	}
	checker, ok := doc["update-checker"].(map[string]any)
	if !ok {
		t.Fatalf("paper-global.yml has no update-checker block:\n%s", files["config/paper-global.yml"])
	}
	enabled, ok := checker["enabled"].(bool)
	if !ok {
		t.Fatalf("update-checker.enabled is %T, want a bool", checker["enabled"])
	}
	return enabled
}

// paperPropertiesDefault is Minecraft's own server.properties, taken from the
// same run as paperGlobalDefault. The timestamp line is kept so a
// regeneration dates itself in the diff.
const paperPropertiesDefault = defaultsDir + "/server.properties.default"

// Minecraft keeps its own default for an unknown key and writes it back out.
func TestServerPropertiesOverlayIsCheckedAgainstMinecraftsOwnKeys(t *testing.T) {
	_, err := Paper(paperValues(), "s3cret", map[string]string{
		// One character off view-distance, which is a real key.
		"server.properties": "view-distanc=12\n",
	})
	if err == nil {
		t.Fatal("Paper accepted a server.properties key Minecraft does not declare")
	}
	for _, want := range []string{"server.properties", "view-distanc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

// Stops the check above from passing on a tree that refuses everything.
func TestServerPropertiesOverlayAcceptsRealKeys(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"server.properties": "difficulty=hard\nview-distance=12\n",
	})
	if err != nil {
		t.Fatalf("Paper refused keys Minecraft declares: %v", err)
	}
	props := parseProperties(string(files["server.properties"]))
	if props["difficulty"] != "hard" || props["view-distance"] != "12" {
		t.Errorf("server.properties = %+v, want the overlay's difficulty and view-distance", props)
	}
}

// An empty or truncated fixture would refuse or admit every key silently.
func TestTheMeasuredPropertiesDefaultIsTheOneTheRendererReads(t *testing.T) {
	raw, err := os.ReadFile(paperPropertiesDefault)
	if err != nil {
		t.Fatalf("read %s: %v", paperPropertiesDefault, err)
	}
	keys := parseProperties(string(raw))
	if len(keys) < 50 {
		t.Fatalf("%s declares %d keys, want a real Minecraft default (about 70)", paperPropertiesDefault, len(keys))
	}
	for _, key := range []string{"server-port", "online-mode", "enable-status", "enforce-secure-profile"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("%s does not declare %q, which this renderer writes unconditionally", paperPropertiesDefault, key)
		}
	}
	for _, key := range []string{"max-players", "motd"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("%s does not declare %q", paperPropertiesDefault, key)
		}
	}
}

// A trailing backslash is a line continuation to Java, and the sort puts
// online-mode right after motd.
func TestATrailingBackslashInAnOverlayValueCannotSwallowTheNextLine(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"server.properties": "motd=hello\\\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	text := string(files["server.properties"])
	if !strings.Contains(text, "motd=hello\\\\\n") {
		t.Errorf("server.properties does not escape the trailing backslash:\n%s", text)
	}
	if !strings.Contains(text, "\nonline-mode=false\n") {
		t.Errorf("online-mode is not on a line of its own:\n%s", text)
	}
}

func TestPropertiesSurviveTheRoundTrip(t *testing.T) {
	in := map[string]string{
		"motd":       "line one\nline two\\ tab\t end\\",
		"level-name": " leading space",
	}
	got := parseProperties(writeProperties(in))
	for k, v := range in {
		if got[k] != v {
			t.Errorf("%q = %q after the round trip, want %q", k, got[k], v)
		}
	}
}
