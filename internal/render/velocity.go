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
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

var VelocityFiles = []string{"velocity.toml"}

// velocityConfigVersion is what the pinned jar validates velocity.toml
// against, read out of its default-velocity.toml (see nix/velocity.nix). A
// Velocity bump that does not re-measure it gets the config migrated out
// from under the renderer.
const velocityConfigVersion = "2.9"

// Velocity renders velocity.toml. Five keys are critical and no overlay can
// move them: bind (the Service targets 25565, not Velocity's 25577),
// online-mode (from ProxyGroup.spec.config.onlineMode only),
// player-info-forwarding-mode (backends verify only "modern"),
// forwarding-secret-file, and advanced.accepts-transfers (from
// spec.update.transfer). secretPath is a path, not the secret: Velocity
// reads the mount itself, and passing the content would put it in plaintext
// into velocity.toml.
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

// velocityToml applies base, overlay, critical by hand because Layer is flat
// and velocity.toml is nested; keep the order the same as Layer's and
// paperGlobal's. A malformed overlay is refused rather than treated as absent.
func velocityToml(v Values, secretPath, overlay string) (string, error) {
	// [servers] stays empty: the agent registers backends over the operator
	// channel. try and [forced-hosts] are spelled out empty because Velocity
	// treats an absent key as its example defaults (try = ["lobby"], forced
	// hosts for *.example.com) and then refuses to start over the servers they
	// name.
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
		// Before the merge, so a key Velocity does not read never reaches the
		// document (haproxy-protocol at the top level instead of under [advanced]
		// is the typical case).
		if err := checkDeclaredKeys(velocityDeclared, fragment, "velocity.toml"); err != nil {
			return "", err
		}
		for k, val := range fragment {
			doc[k] = val
		}
	}

	// An overlay [servers] table replaces ours wholesale and can carry try away,
	// which reopens the startup refusal above, so try is re-defaulted here. A
	// non-table servers is refused: go-toml would marshal it fine and Velocity
	// would refuse to start with nothing naming the overlay.
	servers, ok := doc["servers"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("velocity.toml: servers is a %T, want a table", doc["servers"])
	}
	if _, hasTry := servers["try"]; !hasTry {
		servers["try"] = []string{}
	}
	// forced-hosts has no subkey to lose, but takes the same shape check.
	if _, ok := doc["forced-hosts"].(map[string]any); !ok {
		return "", fmt.Errorf("velocity.toml: forced-hosts is a %T, want a table", doc["forced-hosts"])
	}

	if _, present := doc["advanced"]; !present {
		doc["advanced"] = map[string]any{}
	}
	advanced, ok := doc["advanced"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("velocity.toml: advanced is a %T, want a table", doc["advanced"])
	}

	// Reasserted last. online-mode comes from Values and is written here so no
	// overlay reaches it; paper-global.yml's proxies.velocity.online-mode is a
	// different setting and stays true either way.
	doc["bind"] = "0.0.0.0:25565"
	doc["online-mode"] = *v.OnlineMode
	doc["player-info-forwarding-mode"] = "modern"
	doc["forwarding-secret-file"] = secretPath
	advanced["accepts-transfers"] = v.AcceptsTransfers

	out, err := toml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("velocity.toml: marshalling failed: %w", err)
	}
	return string(out), nil
}
