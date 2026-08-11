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
//     offline-mode;
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
	doc := map[string]any{
		"config-version":   velocityConfigVersion,
		"motd":             valueOr(v.Motd, ""),
		"show-max-players": int64(*v.PlayerLimit),
		"servers":          map[string]any{},
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

	// Reasserted last: whatever the overlay said about these four keys is
	// overwritten rather than merged around.
	doc["bind"] = "0.0.0.0:25565"
	doc["online-mode"] = true
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
