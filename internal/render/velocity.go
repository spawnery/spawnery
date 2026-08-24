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
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// VelocityFiles are the files the Velocity flavour writes, relative to /data.
var VelocityFiles = []string{"velocity.toml"}

// velocityConfigVersion is what the pinned jar validates velocity.toml
// against. Measured out of the jar's own default-velocity.toml — see
// nix/velocity.nix for the version this belongs to and the command that read
// it. A Velocity bump that does not re-measure this produces a config
// Velocity migrates out from under the renderer on first start.
const velocityConfigVersion = "2.8"

// Velocity renders the one file a Spawnery-managed Velocity proxy reads.
//
// Four fields are in the critical layer and no overlay can move them:
//
//   - bind, because internal/podspec names 25565 and the Service targets it
//     by name, not Velocity's own default of 25577;
//   - online-mode, because the proxy is the layer that authenticates players
//     with Mojang and forwards the result — the mirror image of Paper's
//     online-mode=false, and turning it off makes the whole network
//     offline-mode. Critical here means "no configOverlay may reach it", not
//     "constant": its value comes from v.OnlineMode, which is
//     ProxyGroup.spec.config.onlineMode, so switching it off is an edit to
//     the custom resource that an operator reviews rather than a line in a
//     ConfigMap. It is still reasserted after the overlay merge below, so the
//     overlay cannot move it either way;
//   - player-info-forwarding-mode, because anything but "modern" leaves the
//     backends unable to verify a forwarded player;
//   - forwarding-secret-file, so the secret is read from its mount rather
//     than copied into a writable layer — see the secretPath doc below.
//
// secretPath is the forwarding secret's path, not its content: unlike Paper,
// which has no file reference for the secret and must write it into
// paper-global.yml, Velocity's forwarding-secret-file points straight at the
// mount. Passing the secret's content here instead compiles cleanly and
// writes it into velocity.toml in plaintext where the path belongs.
func Velocity(v Values, secretPath string, overlay map[string]string) (map[string][]byte, error) {
	if err := v.RequirePlayerLimit(); err != nil {
		return nil, err
	}
	if err := v.RequireOnlineMode(); err != nil {
		return nil, err
	}
	if secretPath == "" {
		return nil, fmt.Errorf("the forwarding secret path is empty: a proxy with no secret file cannot start modern forwarding")
	}
	if err := checkOverlayFiles(overlay, VelocityFiles); err != nil {
		return nil, err
	}

	doc, err := velocityToml(v, secretPath, overlay["velocity.toml"])
	if err != nil {
		return nil, err
	}

	return map[string][]byte{
		"velocity.toml": []byte(doc),
	}, nil
}

// velocityToml builds velocity.toml's document and marshals it.
//
// This applies base, then overlay, then critical by hand instead of calling
// Layer: Layer is typed map[string]string, and velocity.toml is a nested TOML
// document — [servers] is a table — that would need flattening to dotted
// keys and re-nesting to go through it, which the plan declined in favour of
// duplicating the three-step order here. This must be kept in the same order
// as Layer and as paper.go's paperGlobal, which made the same call for
// paper-global.yml's nested proxies.velocity block — see the note on Layer.
//
// It refuses an overlay that does not parse as TOML, rather than treating it
// as an absent overlay: an overlay that silently does nothing looks exactly
// like one that took effect, and a user who wrote a malformed fragment needs
// to be told, not shipped a proxy that comes up looking healthy while their
// override never applied.
func velocityToml(v Values, secretPath, overlay string) (string, error) {
	// [servers] is added even though it stays empty: the agent registers
	// backends over the operator channel, and a static try list here would
	// be a second truth about which servers exist.
	//
	// try and [forced-hosts] are spelled out empty for a reason that only
	// shows up at runtime, not in a unit test of this string: Velocity does
	// not treat an absent key as "off", it falls back to the same example
	// values documented in its own default-velocity.toml — try = ["lobby"]
	// and forced hosts for lobby.example.com, factions.example.com and
	// minigames.example.com. Against an empty [servers] table those examples
	// name servers that do not exist, and Velocity refuses to start at all
	// ("Fallback server lobby is not registered", "Server 'lobby' for forced
	// host ... does not exist") rather than merely warning. Measured against
	// the pinned jar while building hack/velocity-image-test.sh, which is the
	// first thing in this repository to actually boot Velocity against a
	// rendered file rather than asserting on the string.
	doc := map[string]any{
		"config-version":   velocityConfigVersion,
		"motd":             valueOr(v.Motd, ""),
		"show-max-players": int64(*v.PlayerLimit),
		"servers": map[string]any{
			"try": []string{},
		},
		"forced-hosts": map[string]any{},
	}

	if strings.TrimSpace(overlay) != "" {
		var fragment map[string]any
		if err := toml.Unmarshal([]byte(overlay), &fragment); err != nil {
			return "", fmt.Errorf("velocity.toml: overlay does not parse as TOML: %w", err)
		}
		for k, val := range fragment {
			doc[k] = val
		}
	}

	// doc[k] = val above is whole-key assignment, not a deep merge: an
	// overlay carrying its own [servers] table replaces ours outright and
	// carries the try = [] above away with it, since try is a subkey of
	// servers rather than one of the four critical keys reasserted below.
	// Checked again here rather than assumed to have survived — a missing
	// try is not the same as an empty one to Velocity, which falls back to
	// try = ["lobby"] and reopens the exact startup refusal the base case
	// above exists to close, this time through an ordinary configOverlay.
	//
	// A servers that is not a table is refused rather than skipped, the way
	// paperGlobal refuses a wrong-shaped proxies.velocity. It used to fail
	// this type assertion, skip the re-defaulting in silence, and marshal
	// cleanly — go-toml writes servers = "x" without complaint — so the
	// report a user got was Velocity refusing to start, about a key they had
	// spelled right in a shape they had got wrong, with nothing anywhere
	// naming the overlay.
	servers, ok := doc["servers"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("velocity.toml: servers is a %T, want a table", doc["servers"])
	}
	if _, hasTry := servers["try"]; !hasTry {
		servers["try"] = []string{}
	}
	// forced-hosts has no such subkey to lose, so there is nothing to
	// re-default — but it takes the same shape check for the same reason.
	// What stood here was a presence check that could never fire: the base
	// doc above always sets the key and the overlay loop can only overwrite
	// an existing key, never delete one, so it was true by construction on
	// every call, and a string would have satisfied it anyway.
	if _, ok := doc["forced-hosts"].(map[string]any); !ok {
		return "", fmt.Errorf("velocity.toml: forced-hosts is a %T, want a table", doc["forced-hosts"])
	}

	// Reasserted last: whatever the overlay said about these four keys is
	// overwritten rather than merged around.
	//
	// online-mode takes its value from Values rather than a literal, and is
	// still written here rather than in the base document above, so a
	// configOverlay cannot reach it in either direction — the switch is
	// ProxyGroup.spec.config.onlineMode and nothing else. Nothing on the Paper
	// side moves with it: paper-global.yml's proxies.velocity.online-mode is a
	// different setting under an almost identical name ("trust what the proxy
	// forwards", see paperGlobal's comment) and stays true, because modern
	// forwarding works the same whether the proxy authenticated the player or
	// just made a UUID up.
	doc["bind"] = "0.0.0.0:25565"
	doc["online-mode"] = *v.OnlineMode
	doc["player-info-forwarding-mode"] = "modern"
	doc["forwarding-secret-file"] = secretPath

	// go-toml/v2 prefers TOML's literal (single-quoted) string form over the
	// basic (double-quoted) one whenever a value permits it; both are the
	// same value to any TOML parser, Velocity's included, so no attempt is
	// made here to force one style over the other — see
	// TestVelocityEscapesAMotdThatCannotBeALiteralString for the one place
	// that distinction is actually load-bearing (a motd that cannot be a
	// literal string at all).
	out, err := toml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("velocity.toml: marshalling failed: %w", err)
	}
	return string(out), nil
}
