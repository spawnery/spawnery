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
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// PaperFiles are the files the Paper flavour writes, relative to /data.
var PaperFiles = []string{"server.properties", "config/paper-global.yml"}

// paperOverlayKeys are the ConfigMap keys an overlay may set. They are not
// PaperFiles: a Kubernetes ConfigMap key cannot contain '/', so
// "config/paper-global.yml" could never be a key an overlay actually has —
// the overlay names the file "paper-global.yml", the same bare name Paper
// reads it by below, and checkOverlayFiles must be told that, not the
// /data-relative write path.
var paperOverlayKeys = []string{"server.properties", "paper-global.yml"}

// Paper renders the two files a Spawnery-managed Paper server reads.
//
// The three fields in server.properties that the operator relies on are in the
// critical layer and no overlay can move them:
//
//   - server-port, because internal/podspec names 25565 and a pod whose
//     process listens elsewhere passes no probe;
//   - online-mode=false, because the proxy authenticates players and forwards
//     the result — with it on, modern forwarding fails every join;
//   - enable-status, because with it off the server answers no server list
//     ping, the readiness probe stays red forever, and nothing in the log says
//     why.
func Paper(v Values, secret string, overlay map[string]string) (map[string][]byte, error) {
	if err := v.RequireMaxPlayers(); err != nil {
		return nil, err
	}
	if secret == "" {
		return nil, fmt.Errorf("the forwarding secret is empty: a backend with online-mode=false and no secret is joinable by anyone")
	}
	if err := checkOverlayFiles(overlay, paperOverlayKeys); err != nil {
		return nil, err
	}

	props := Layer(
		map[string]string{
			"max-players": strconv.FormatInt(int64(*v.MaxPlayers), 10),
			"motd":        valueOr(v.Motd, ""),
		},
		parseProperties(overlay["server.properties"]),
		map[string]string{
			"server-port":            "25565",
			"online-mode":            "false",
			"enable-status":          "true",
			"enforce-secure-profile": "false",
		},
	)

	global, err := paperGlobal(secret, overlay["paper-global.yml"])
	if err != nil {
		return nil, err
	}

	return map[string][]byte{
		"server.properties":       []byte(writeProperties(props)),
		"config/paper-global.yml": []byte(global),
	}, nil
}

// checkOverlayFiles refuses an overlay that names a file the flavour does not
// write. An overlay key that silently falls on the floor is worse than an
// error: the operator believes it applied and the cluster does not reflect
// it, and nothing short of reading the rendered ConfigMap byte-for-byte would
// reveal the mistake.
func checkOverlayFiles(overlay map[string]string, files []string) error {
	known := make(map[string]bool, len(files))
	for _, f := range files {
		known[f] = true
	}
	for name := range overlay {
		if !known[name] {
			return fmt.Errorf("overlay names %q, which this flavour does not write", name)
		}
	}
	return nil
}

// valueOr dereferences a pointer field or returns a fallback for an absent
// one. Unlike MaxPlayers, the fields that go through this helper have no
// operator-visible consequence when unset, so there is no refusal to write —
// only a default to pick.
func valueOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

// paperGlobal writes the Velocity block of paper-global.yml.
//
// proxies.velocity.online-mode: true reads as the opposite of
// server.properties' online-mode=false and both are correct at once: the
// properties flag says "do not authenticate players yourself", this one says
// "trust the authentication result Velocity forwards". Setting this one false
// while the other is false too gives every player an offline-mode UUID, which
// silently detaches them from their own inventories.
//
// It refuses an overlay that does not parse as YAML, and refuses one whose
// proxies or proxies.velocity key is not a mapping, rather than treating
// either as an absent overlay: an overlay that silently does nothing looks
// exactly like one that took effect, and a user who wrote a wrong-shaped
// block needs to be told, not shipped a server that comes up looking healthy
// while their override never applied.
//
// This applies base, then overlay, then critical by hand instead of calling
// Layer: Layer is typed map[string]string, and proxies.velocity is a nested
// document that would need flattening to dotted keys and re-nesting to use
// it, which the plan declined in favour of duplicating the three-step order
// here. The two implementations must be kept in the same order — see the
// note on Layer.
func paperGlobal(secret, overlay string) (string, error) {
	// proxies.velocity is the only nested structure this file writes, so the
	// overlay is applied at exactly that depth rather than through a generic
	// deep merge: three keys do not earn a nester.
	velocity := map[string]any{}
	if strings.TrimSpace(overlay) != "" {
		var fragment map[string]any
		if err := yaml.Unmarshal([]byte(overlay), &fragment); err != nil {
			return "", fmt.Errorf("paper-global.yml: overlay does not parse as YAML: %w", err)
		}
		if raw, ok := fragment["proxies"]; ok {
			proxies, ok := raw.(map[string]any)
			if !ok {
				return "", fmt.Errorf("paper-global.yml: proxies is a %T, want a mapping", raw)
			}
			if raw, ok := proxies["velocity"]; ok {
				v, ok := raw.(map[string]any)
				if !ok {
					return "", fmt.Errorf("paper-global.yml: proxies.velocity is a %T, want a mapping", raw)
				}
				for k, val := range v {
					velocity[k] = val
				}
			}
		}
	}

	// Reasserted last: whatever the overlay said about these three keys is
	// overwritten rather than merged around.
	velocity["enabled"] = true
	velocity["online-mode"] = true
	velocity["secret-key"] = secret

	out, err := yaml.Marshal(map[string]any{
		"proxies": map[string]any{"velocity": velocity},
	})
	if err != nil {
		// The document is built entirely from strings, bools and maps of the
		// same, which yaml.Marshal always accepts — there is no input that
		// reaches this branch.
		panic(fmt.Sprintf("paperGlobal: marshalling a known-good document failed: %v", err))
	}
	return string(out), nil
}

// parseProperties reads a .properties fragment into a flat key set. Blank
// lines and comments are skipped; a line with no '=' is skipped rather than
// failing, because a properties file with a stray line is still a properties
// file and refusing one would turn a harmless overlay into a crash loop.
func parseProperties(fragment string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(fragment, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

// writeProperties serialises a flat key set.
//
// Sorted, because Go map iteration is randomised: an unsorted file changes its
// bytes on every render, which breaks nothing at runtime and makes every diff
// between two pods useless for telling whether their configuration differs.
func writeProperties(props map[string]string) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, props[k])
	}
	return b.String()
}
