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

// containsTOMLString reports whether rendered sets key to value, written as
// either of TOML's two equivalent string forms — the literal ('...') form
// go-toml/v2 prefers when a value permits it, or the basic ("...") form.
// Both parse to the identical value in any TOML reader, Velocity's included,
// so a test asserting the rendered *behaviour* has no business pinning one
// spelling over the other.
func containsTOMLString(rendered, key, value string) bool {
	return strings.Contains(rendered, key+` = "`+value+`"`) ||
		strings.Contains(rendered, key+` = '`+value+`'`)
}

// velocityDefault is Velocity's own default-velocity.toml, byte for byte as
// the pinned jar ships it. Velocity writes this file out when /data has none,
// and it is the same document its config loader validates a rendered one
// against — so it is a measurement of the receiving program, which is the only
// reason it exists. Every other test in this file asserts that the renderer
// writes the string the renderer says it writes, and no such test can fail on
// a key Velocity does not read.
//
// It costs an extraction rather than a server boot, because Velocity ships it
// as a jar resource. Reproduce it with:
//
//	JAR=$(nix build .#velocity-jar --no-link --print-out-paths)
//	cd internal/render/defaults && jar xf "$JAR" default-velocity.toml
//	mv default-velocity.toml velocity.default.toml
//
// (default-velocity.toml sits at the jar root, not under META-INF; `unzip` is
// not on PATH in the dev shell, so `jar xf` extracts it. This is the same
// command nix/velocity.nix's config-version comment records, against the same
// pin.)
//
// A Velocity bump therefore has to re-run this and update the file, exactly
// the way a Paper bump has to re-run internal/render/defaults'
// paper-global.default.yml.
const velocityDefault = defaultsDir + "/velocity.default.toml"

// The keys this renderer writes have to be keys Velocity declares.
//
// Velocity does not refuse a key it does not know; night-config parses the
// file and the loader reads out the keys it asks for, so a misspelling is a
// key nobody reads and a default silently kept. Two shapes of that are not
// theoretical:
//
//   - forwarding-secret-file misspelled: Velocity finds no such key, falls
//     back to its own relative "forwarding.secret", finds no such file,
//     creates one in the writable /data, fills it with twelve random
//     characters, logs "The forwarding-secret-file does not exist. A new file
//     has been created at {}", starts cleanly, passes the port probe — and
//     refuses every forwarded join, because the backends carry a different
//     secret. Disassembled out of the pinned jar's
//     VelocityConfiguration.read, 2026-08-11.
//   - show-max-players misspelled: Velocity's own default is 500 and
//     podspec.DefaultPlayerLimit is 500, so on the default path there is
//     nothing to see at all.
//
// This is the Velocity half of the lesson milestone 3c learned on the Paper
// side, where render.Paper wrote proxies.velocity.secret-key for a reader that
// wanted secret — see TestPaperWritesTheKeysPaperItselfReads, which this
// mirrors.
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

// The config-version the renderer writes is the one the pinned jar ships, and
// this is the only place the two are compared. velocityConfigVersion is a
// constant measured by hand out of the jar; the fixture is that same file
// checked in. A Velocity bump that regenerates the fixture without moving the
// constant fails here instead of producing a config Velocity migrates out from
// under the renderer on first start.
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

// velocityTomlKeysOf reads the key names out of a velocity.toml document: every
// top-level key, plus servers.try. It fails rather than returning an empty set
// when the document has no keys, so a truncated fixture cannot pass by having
// nothing to compare.
//
// [servers] is the one table whose keys are not Velocity's to declare — each
// is a server name somebody chose, and the fixture's are Velocity's three
// example servers — so only try, the reserved key in there, is carried
// through. [forced-hosts] is the same shape and contributes nothing but its
// own name. Nothing else the renderer writes nests.
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

// The proxy half of the inversion: true here, false on the backends.
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

// And the value actually travels, rather than the renderer reading
// v.OnlineMode and writing true anyway. Without this the field would be a
// setting that exists on the CRD, appears in config.yaml, and does nothing —
// which is the failure mode the caller would only find by trying to join with
// an unauthenticated client and being told to log in.
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

// A config.yaml that says nothing about online-mode is refused rather than
// guessed at, the way an absent playerLimit is. Both defaults would be wrong:
// true silently overrides an operator who chose false, false silently opens
// the network to anyone under any name.
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

// The overlay still cannot reach online-mode, and the direction that matters
// most is the one the four-key test above cannot cover: with the value set to
// false, an overlay must not be able to turn authentication back on either.
// online-mode moved from a literal to a value read out of Values, and a
// renderer that set it in the base document instead of after the merge would
// pass every other test in this file while handing a configOverlay control of
// whether the network authenticates anyone.
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

// 25565, not Velocity's own default of 25577: internal/podspec names 25565 and
// the Service targets it by name.
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

// The agent registers backends over the operator channel. A static list here
// would be a second truth about which servers exist.
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

// try and forced-hosts have to be spelled out empty, not merely absent: a
// missing key falls back to Velocity's own built-in example (try = ["lobby"]
// and three forced hosts naming servers this proxy never declares), which
// refuses to start against an empty [servers] table rather than warning and
// continuing. hack/velocity-image-test.sh is what actually caught this — a
// unit test asserting on the rendered string cannot tell "absent" from
// "explicitly empty" the way Velocity's own config loader does.
//
// This covers only the no-overlay case; see
// TestVelocityOverlayServersTableKeepsAnEmptyTry for the one where an
// overlay's own [servers] table would otherwise carry try away with it.
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

// doc[k] = val in velocityToml is whole-key assignment, not a deep merge: an
// overlay that declares its own [servers] table — to add a server, say —
// replaces the base table outright, including the try = [] subkey nested
// inside it. Without the post-overlay check this test guards, that overlay
// would silently reopen the startup refusal
// TestVelocityDefaultsTryAndForcedHostsEmptyWithNoOverlay closes: Velocity
// falls back to try = ["lobby"], which does not exist in this rendered file
// either.
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

// An overlay that tries to move any of the four critical keys must lose to
// the reassertion at the end of velocityToml — the Velocity analogue of
// TestPaperOverlayCannotMoveVelocityCriticalKeys. All four are attacked in
// one overlay and all four are asserted, because they are set together in
// one block in velocityToml: a test that only attacked bind and online-mode
// would stay green if a later edit dropped the line reasserting
// player-info-forwarding-mode or forwarding-secret-file, which is exactly
// the class of regression this test exists to catch.
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

// TOML's literal ('...') and basic ("...") string forms stop being
// interchangeable exactly when a value contains a single quote or a control
// character such as a newline: a literal string cannot express either at
// all, so a correct encoder must fall back to a basic string and escape the
// value. This is the one place the quoting distinction the earlier tests
// deliberately ignore is actually load-bearing, and the failure it would
// catch is a real encoding bug rather than a style choice — so instead of
// asserting anything about which quote character was used, this parses the
// rendered file back with the same library and checks the motd survives.
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

// A [servers] or [forced-hosts] an overlay turned into something other than a
// table is refused, not skipped.
//
// servers is the one that had teeth: the type assertion above the try
// re-defaulting used to fail quietly, so the empty try list this renderer
// exists to keep alive was dropped, go-toml marshalled `servers = "x"` without
// complaint, and the whole report was Velocity refusing to start — about a key
// the user had spelled right in a shape they had got wrong, with nothing
// anywhere naming the overlay. forced-hosts had a presence check that could
// never fire and would have been satisfied by a string in any case.
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

// The RKE2 rollout's half day, made into a render-time error.
//
// Velocity's haproxy-protocol lives under [advanced]. Set at the top level it
// reached the rendered /data/velocity.toml — where it read exactly as intended
// — and Velocity behaved as though it were false: no PROXY header required,
// and a connection carrying one dropped without a log line. Nothing in the
// operator knew Velocity's schema, so a misplaced key was indistinguishable
// from a correct one until something downstream behaved strangely.
//
// The error has to say where the key does live, because that is the shape both
// of this project's overlay outages took: a real key at the wrong depth, not
// an invented one.
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

// The same key in the right place is the overlay a real cluster runs, so it
// must keep working. Without this the test above would be satisfied by a check
// that refuses everything.
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

// [servers] and [forced-hosts] are keyed by names somebody chose, so the check
// must not measure those against the fixture's three example servers.
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

// A key that is nowhere in the document at all gets the other half of the
// message: what the level it was written at does declare.
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
