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
	for _, want := range []string{"enabled: true", "online-mode: true", "s3cret"} {
		if !strings.Contains(global, want) {
			t.Errorf("paper-global.yml does not contain %q:\n%s", want, global)
		}
	}
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
		"paper-global.yml": "proxies:\n  velocity:\n    enabled: false\n    online-mode: false\n    secret-key: not-the-real-secret\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	global := string(files["config/paper-global.yml"])
	for _, want := range []string{"enabled: true", "online-mode: true", "secret-key: s3cret"} {
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

// A well-formed overlay field that this file does not model on its own must
// still reach the output, the paper-global.yml analogue of
// TestPaperOverlayReachesAnUnmodelledField — otherwise the malformed-overlay
// refusal below could be masking a fix that never actually lets a legitimate
// overlay through.
func TestPaperOverlayReachesPaperGlobal(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-global.yml": "proxies:\n  velocity:\n    announce-forwarding: true\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	global := string(files["config/paper-global.yml"])
	if !strings.Contains(global, "announce-forwarding: true") {
		t.Errorf("the overlay did not reach paper-global.yml:\n%s", global)
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
