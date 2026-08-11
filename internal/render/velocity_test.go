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
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func velocityValues() Values {
	n := int32(500)
	m := "A Spawnery network"
	return Values{PlayerLimit: &n, Motd: &m}
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
	n := int32(500)
	m := "A 'Spawnery' network\\with a backslash\nand a newline"
	files, err := Velocity(Values{PlayerLimit: &n, Motd: &m}, testSecretPath, nil)
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
