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
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func velocityValues() Values {
	n := int32(500)
	m := "A Spawnery network"
	online := true
	return Values{PlayerLimit: &n, Motd: &m, OnlineMode: &online}
}

const testSecretPath = "/etc/spawnery/forwarding.secret"

// containsTOMLString accepts both TOML string forms; go-toml/v2 picks the
// literal one when it can.
func containsTOMLString(rendered, key, value string) bool {
	return strings.Contains(rendered, key+` = "`+value+`"`) ||
		strings.Contains(rendered, key+` = '`+value+`'`)
}

// velocityDefault is Velocity's own default-velocity.toml from the pinned jar.
// A Velocity bump has to regenerate it with:
//
//	JAR=$(nix build .#velocity-jar --no-link --print-out-paths)
//	cd internal/render/defaults && jar xf "$JAR" default-velocity.toml
//	mv default-velocity.toml velocity.default.toml
const velocityDefault = defaultsDir + "/velocity.default.toml"

// Velocity never refuses an unknown key. A misspelled forwarding-secret-file
// makes it generate a random secret in /data and reject every forwarded
// join; a misspelled show-max-players is invisible, since both defaults are
// 500.
func TestVelocityWritesTheKeysVelocityItselfReads(t *testing.T) {
	defaults, err := os.ReadFile(velocityDefault)
	if err != nil {
		t.Fatalf("read Velocity's own defaults: %v", err)
	}
	declared := velocityTomlKeysOf(t, defaults, velocityDefault)

	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	rendered := files["velocity.toml"]

	for _, key := range sortedKeys(velocityTomlKeysOf(t, rendered, "the rendered velocity.toml")) {
		if !declared[key] {
			t.Errorf("the renderer writes %s, which Velocity does not declare; Velocity reads %v and would silently keep its own default for whatever this key was meant to set:\n%s",
				key, sortedKeys(declared), rendered)
		}
	}
}

// The only place velocityConfigVersion is compared with the fixture.
func TestVelocityWritesThePinnedConfigVersion(t *testing.T) {
	defaults, err := os.ReadFile(velocityDefault)
	if err != nil {
		t.Fatalf("read Velocity's own defaults: %v", err)
	}
	var parsed struct {
		ConfigVersion string `toml:"config-version"`
	}
	if err := toml.Unmarshal(defaults, &parsed); err != nil {
		t.Fatalf("%s does not parse as TOML: %v", velocityDefault, err)
	}
	if parsed.ConfigVersion != velocityConfigVersion {
		t.Errorf("velocityConfigVersion is %q, the pinned jar's default-velocity.toml says %q",
			velocityConfigVersion, parsed.ConfigVersion)
	}
}

// velocityTomlKeysOf returns the top-level keys, servers.try and everything
// under [advanced]; other [servers] and [forced-hosts] keys are user names.
// It fails on an empty document, so a truncated fixture cannot pass.
func velocityTomlKeysOf(t *testing.T, doc []byte, what string) map[string]bool {
	t.Helper()
	var parsed map[string]any
	if err := toml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("%s does not parse as TOML: %v", what, err)
	}
	if len(parsed) == 0 {
		t.Fatalf("%s has no keys at all:\n%s", what, doc)
	}
	keys := make(map[string]bool, len(parsed))
	for k, v := range parsed {
		keys[k] = true
		if table, ok := v.(map[string]any); ok && k == "advanced" {
			for sub := range table {
				keys["advanced."+sub] = true
			}
		}
		if k != "servers" {
			continue
		}
		if table, ok := v.(map[string]any); ok {
			if _, hasTry := table["try"]; hasTry {
				keys["servers.try"] = true
			}
		}
	}
	return keys
}

func TestVelocityKeepsOnlineModeOn(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, "online-mode = true") {
		t.Errorf("velocity.toml does not keep online-mode on:\n%s", toml)
	}
}

func TestVelocityTurnsOnlineModeOffWhenTheValueSaysSo(t *testing.T) {
	v := velocityValues()
	off := false
	v.OnlineMode = &off

	files, err := Velocity(v, testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, "online-mode = false") {
		t.Errorf("velocity.toml does not carry onlineMode: false through; the proxy still authenticates and no offline client can join:\n%s", toml)
	}
}

func TestVelocityRefusesAnUnsetOnlineMode(t *testing.T) {
	v := velocityValues()
	v.OnlineMode = nil

	_, err := Velocity(v, testSecretPath, nil)
	if err == nil {
		t.Fatal("an unset onlineMode was accepted")
	}
	if !strings.Contains(err.Error(), "onlineMode") {
		t.Errorf("error = %q, want it to name the key", err)
	}
}

// With the value false, an overlay must not turn authentication back on
// either; a renderer setting online-mode before the merge would allow it.
func TestVelocityOverlayCannotMoveOnlineModeInEitherDirection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		value        bool
		overlaySays  string
		wantRendered string
	}{
		{"an overlay cannot turn it off", true, "online-mode = false\n", "online-mode = true"},
		{"an overlay cannot turn it on", false, "online-mode = true\n", "online-mode = false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := velocityValues()
			value := tc.value
			v.OnlineMode = &value

			files, err := Velocity(v, testSecretPath, map[string]string{"velocity.toml": tc.overlaySays})
			if err != nil {
				t.Fatalf("Velocity: %v", err)
			}
			rendered := string(files["velocity.toml"])
			if !strings.Contains(rendered, tc.wantRendered) {
				t.Errorf("velocity.toml does not contain %q; the overlay moved online-mode:\n%s", tc.wantRendered, rendered)
			}
		})
	}
}

func TestVelocityBindsThePortThePodspecNames(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	if !containsTOMLString(string(files["velocity.toml"]), "bind", "0.0.0.0:25565") {
		t.Errorf("velocity.toml does not bind 25565:\n%s", files["velocity.toml"])
	}
}

func TestVelocityPointsAtTheSecretFileRatherThanCopyingIt(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	rendered := string(files["velocity.toml"])
	if !containsTOMLString(rendered, "forwarding-secret-file", testSecretPath) {
		t.Errorf("velocity.toml does not point at the mounted secret:\n%s", rendered)
	}
	if strings.Contains(rendered, "forwarding-secret =") {
		t.Error("velocity.toml carries the secret inline; it must only reference the file")
	}
}

func TestVelocityUsesModernForwarding(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	if !containsTOMLString(string(files["velocity.toml"]), "player-info-forwarding-mode", "modern") {
		t.Error("velocity.toml is not on modern forwarding")
	}
}

func TestVelocityShipsNoServers(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, "[servers]") {
		t.Error("velocity.toml has no [servers] table at all; Velocity needs the table even when it is empty")
	}
	if strings.Contains(toml, "try = [\"") {
		t.Errorf("velocity.toml ships a non-empty try list:\n%s", toml)
	}
}

// Absent is not empty to Velocity: it falls back to its examples and refuses
// to start. Only hack/velocity-image-test.sh can see the difference at
// runtime.
func TestVelocityDefaultsTryAndForcedHostsEmptyWithNoOverlay(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, "try = []") {
		t.Errorf("velocity.toml does not spell out an empty try list:\n%s", toml)
	}
	if !strings.Contains(toml, "[forced-hosts]") {
		t.Errorf("velocity.toml has no [forced-hosts] table at all:\n%s", toml)
	}
}

// An overlay [servers] table replaces the base one wholesale, try included.
func TestVelocityOverlayServersTableKeepsAnEmptyTry(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "[servers]\n" + `lobby-external = "10.0.0.5:25565"` + "\n",
	})
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, "try = []") {
		t.Errorf("an overlay [servers] table dropped the empty try list:\n%s", toml)
	}
	if !strings.Contains(toml, `lobby-external = "10.0.0.5:25565"`) &&
		!strings.Contains(toml, `lobby-external = '10.0.0.5:25565'`) {
		t.Errorf("the overlay's own server entry did not reach velocity.toml:\n%s", toml)
	}
}

func TestVelocityCarriesTheMotdAndLimit(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, "A Spawnery network") {
		t.Error("the motd did not reach velocity.toml")
	}
	if !strings.Contains(toml, "show-max-players = 500") {
		t.Error("the player limit did not reach velocity.toml")
	}
}

// All four attacked and asserted at once, so dropping any one reassertion
// line fails.
func TestVelocityRefusesATransferGroupWhoseOverlayPredatesTransfers(t *testing.T) {
	values := velocityValues()
	values.AcceptsTransfers = true
	_, err := Velocity(values, testSecretPath, map[string]string{
		"velocity.toml": `config-version = "2.6"` + "\n",
	})
	if err == nil {
		t.Fatal("Velocity accepted an overlay at config-version 2.6 for a group with transfer; Velocity's migration would switch accepts-transfers off")
	}
	for _, want := range []string{"config-version", "2.6", "accepts-transfers"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

func TestVelocityKeepsAnOldOverlayVersionWithoutTransfer(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": `config-version = "2.6"` + "\n",
	})
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	if !containsTOMLString(string(files["velocity.toml"]), "config-version", "2.6") {
		t.Errorf("the overlay's config-version did not reach velocity.toml:\n%s", files["velocity.toml"])
	}
}

func TestVelocityAcceptsACurrentOverlayVersionWithTransfer(t *testing.T) {
	values := velocityValues()
	values.AcceptsTransfers = true
	for _, version := range []string{"2.7", velocityConfigVersion} {
		if _, err := Velocity(values, testSecretPath, map[string]string{
			"velocity.toml": `config-version = "` + version + `"` + "\n",
		}); err != nil {
			t.Errorf("config-version %s: %v", version, err)
		}
	}
}

func TestVelocityOverlayCannotMoveCriticalKeys(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "online-mode = false\n" +
			`bind = "0.0.0.0:1234"` + "\n" +
			`player-info-forwarding-mode = "legacy"` + "\n" +
			`forwarding-secret-file = "/tmp/not-the-real-secret"` + "\n",
	})
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	rendered := string(files["velocity.toml"])

	if !strings.Contains(rendered, "online-mode = true") {
		t.Errorf("velocity.toml does not contain online-mode = true, the overlay moved a critical key:\n%s", rendered)
	}
	if !containsTOMLString(rendered, "bind", "0.0.0.0:25565") {
		t.Errorf("velocity.toml does not contain the critical bind, the overlay moved it:\n%s", rendered)
	}
	if !containsTOMLString(rendered, "player-info-forwarding-mode", "modern") {
		t.Errorf("velocity.toml does not contain the critical forwarding mode, the overlay moved it:\n%s", rendered)
	}
	if !containsTOMLString(rendered, "forwarding-secret-file", testSecretPath) {
		t.Errorf("velocity.toml does not contain the critical secret path, the overlay moved it:\n%s", rendered)
	}

	for _, unwanted := range []string{"online-mode = false", "0.0.0.0:1234", "legacy", "not-the-real-secret"} {
		if strings.Contains(rendered, unwanted) {
			t.Errorf("velocity.toml contains %q, an overlay value that should have been clobbered:\n%s", unwanted, rendered)
		}
	}
}

func TestVelocityOverlayReachesAnUnmodelledField(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "kick-existing-players = true\n",
	})
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	if !strings.Contains(string(files["velocity.toml"]), "kick-existing-players = true") {
		t.Error("the overlay did not reach velocity.toml")
	}
}

func TestVelocityRefusesAnOverlayForAFileItDoesNotWrite(t *testing.T) {
	_, err := Velocity(velocityValues(), testSecretPath, map[string]string{"server.properties": "x=1\n"})
	if err == nil {
		t.Fatal("an overlay for a foreign file was accepted")
	}
	if !strings.Contains(err.Error(), "server.properties") {
		t.Errorf("error = %q, want it to name the file", err)
	}
}

// A single quote or newline cannot be a TOML literal string, so this parses
// the output back instead of asserting a quote style.
func TestVelocityEscapesAMotdThatCannotBeALiteralString(t *testing.T) {
	v := velocityValues()
	m := "A 'Spawnery' network\\with a backslash\nand a newline"
	v.Motd = &m
	files, err := Velocity(v, testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	rendered := files["velocity.toml"]

	var decoded struct {
		Motd string `toml:"motd"`
	}
	if err := toml.Unmarshal(rendered, &decoded); err != nil {
		t.Fatalf("velocity.toml does not parse as TOML: %v\n%s", err, rendered)
	}
	if decoded.Motd != m {
		t.Errorf("motd round-tripped as %q, want %q\n%s", decoded.Motd, m, rendered)
	}
}

func TestVelocityRefusesAMisshapenServersTable(t *testing.T) {
	for _, tc := range []struct{ name, overlay, key string }{
		{"servers", "servers = \"lobby\"\n", "servers"},
		{"forced-hosts", "forced-hosts = 3\n", "forced-hosts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Velocity(velocityValues(), testSecretPath, map[string]string{
				"velocity.toml": tc.overlay,
			})
			if err == nil {
				t.Fatalf("an overlay whose %s is not a table was accepted", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error = %q, want it to name %q", err, tc.key)
			}
			if !strings.Contains(err.Error(), "want a table") {
				t.Errorf("error = %q, want it to name the shape problem, not just the key", err)
			}
		})
	}
}

// haproxy-protocol belongs under [advanced]; at the top level Velocity reads
// it as false without a word. The error must name where the key belongs.
func TestVelocityRefusesAKeyAtTheWrongDepth(t *testing.T) {
	_, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "haproxy-protocol = true\n",
	})
	if err == nil {
		t.Fatal("an overlay setting haproxy-protocol at the top level was accepted")
	}
	if !strings.Contains(err.Error(), "advanced.haproxy-protocol") {
		t.Errorf("error = %q, want it to say where the key is actually declared — "+
			"a list of top-level keys leaves the author to spot it", err)
	}
}

// Without this the test above would pass on a check that refuses everything.
func TestVelocityAcceptsTheSameKeyWhereItBelongs(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "[advanced]\nhaproxy-protocol = true\n",
	})
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	if !strings.Contains(string(files["velocity.toml"]), "haproxy-protocol = true") {
		t.Errorf("the overlay did not reach velocity.toml:\n%s", files["velocity.toml"])
	}
}

func TestVelocityAcceptsNamesTheUserChose(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "[servers]\nsurvival = \"10.0.0.5:25565\"\n" +
			"[forced-hosts]\n\"survival.example.com\" = [\"survival\"]\n",
	})
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	rendered := string(files["velocity.toml"])
	for _, want := range []string{"survival", "survival.example.com"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the overlay did not reach velocity.toml, %q missing:\n%s", want, rendered)
		}
	}
}

func TestVelocityRefusesAKeyItHasNeverHeardOf(t *testing.T) {
	_, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "[advanced]\nhaproxy-protokol = true\n",
	})
	if err == nil {
		t.Fatal("an overlay setting a misspelt key under [advanced] was accepted")
	}
	if !strings.Contains(err.Error(), "haproxy-protocol") {
		t.Errorf("error = %q, want the keys [advanced] does declare, which is how a "+
			"misspelling is spotted", err)
	}
}

func TestVelocityCarriesAcceptsTransfers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   bool
		overlay string
		want    bool
	}{
		{"off without an overlay", false, "", false},
		{"on without an overlay", true, "", true},
		{"an overlay cannot turn it on", false, "[advanced]\naccepts-transfers = true\n", false},
		{"an overlay cannot turn it off", true, "[advanced]\naccepts-transfers = false\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := velocityValues()
			v.AcceptsTransfers = tc.value
			files, err := Velocity(v, testSecretPath, map[string]string{"velocity.toml": tc.overlay})
			if err != nil {
				t.Fatalf("Velocity: %v", err)
			}
			var doc struct {
				Advanced map[string]any `toml:"advanced"`
			}
			if err := toml.Unmarshal(files["velocity.toml"], &doc); err != nil {
				t.Fatalf("velocity.toml does not parse: %v", err)
			}
			got, ok := doc.Advanced["accepts-transfers"].(bool)
			if !ok || got != tc.want {
				t.Errorf("advanced.accepts-transfers = %v (present %v), want %v:\n%s",
					doc.Advanced["accepts-transfers"], ok, tc.want, files["velocity.toml"])
			}
		})
	}
}

func TestVelocityKeepsAnOverlaysOtherAdvancedKeys(t *testing.T) {
	v := velocityValues()
	v.AcceptsTransfers = true
	files, err := Velocity(v, testSecretPath, map[string]string{
		"velocity.toml": "[advanced]\nhaproxy-protocol = true\n",
	})
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	rendered := string(files["velocity.toml"])
	for _, want := range []string{"haproxy-protocol = true", "accepts-transfers = true"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("velocity.toml does not contain %q:\n%s", want, rendered)
		}
	}
}

func TestVelocityRefusesAMisshapenAdvancedTable(t *testing.T) {
	_, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "advanced = \"x\"\n",
	})
	if err == nil {
		t.Fatal("an advanced that is not a table was accepted")
	}
}
