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
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// PaperFiles are the files the Paper flavour may write, relative to /data.
// config/paper-world-defaults.yml is written only when an overlay names it:
// an empty write on every start would erase what a persistent server filled
// in itself.
var PaperFiles = []string{
	"server.properties",
	"config/paper-global.yml",
	"config/paper-world-defaults.yml",
}

// paperOverlayKeys are bare names, because a ConfigMap key cannot contain '/'.
var paperOverlayKeys = []string{
	"server.properties",
	"paper-global.yml",
	"paper-world-defaults.yml",
}

// Paper renders the files a Paper server reads. Four server.properties keys
// are critical and no overlay can move them: server-port (the probe dials
// 25565), online-mode=false (the proxy authenticates; on, modern forwarding
// fails every join), enable-status (off, the SLP readiness probe stays red
// with nothing in the log), and enforce-secure-profile=false (on, Paper
// refuses every join without a Mojang-signed chat session, which
// online-mode=false never has).
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

	// Before the layering, so a key Minecraft does not read is reported instead
	// of silently added to the file and ignored.
	userProps := parseProperties(overlay["server.properties"])
	if len(userProps) > 0 {
		doc := make(map[string]any, len(userProps))
		for k, val := range userProps {
			doc[k] = val
		}
		if err := checkDeclaredKeys(paperPropertiesDeclared, doc, "server.properties"); err != nil {
			return nil, err
		}
	}

	props := Layer(
		map[string]string{
			"max-players": strconv.FormatInt(int64(*v.MaxPlayers), 10),
			"motd":        valueOr(v.Motd, ""),
			// Minecraft 26.3 turned its own default to true, which refuses
			// every player on a server that starts with an empty whitelist.
			"white-list": "false",
		},
		userProps,
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

	files := map[string][]byte{
		"server.properties":       []byte(writeProperties(props)),
		"config/paper-global.yml": []byte(global),
	}

	// Only when the overlay names it; see PaperFiles.
	if raw, ok := overlay["paper-world-defaults.yml"]; ok {
		doc, err := paperWorldDefaults(raw)
		if err != nil {
			return nil, err
		}
		files["config/paper-world-defaults.yml"] = []byte(doc)
	}

	return files, nil
}

// paperWorldDefaults returns the overlay itself, checked for undeclared keys
// (Paper keeps a stray key and writes it back, looking applied) and
// re-marshalled so that a non-mapping document is refused here.
func paperWorldDefaults(overlay string) (string, error) {
	if strings.TrimSpace(overlay) == "" {
		// An empty overlay means "leave this file alone".
		return "", nil
	}

	// Declared nil, not initialised: unmarshalling YAML null into an allocated
	// map leaves it unchanged, so only a nil map can catch an overlay of "null".
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(overlay), &doc); err != nil {
		return "", fmt.Errorf("paper-world-defaults.yml: overlay does not parse as YAML: %w", err)
	}
	if doc == nil {
		return "", fmt.Errorf("paper-world-defaults.yml: overlay parses to nothing; " +
			"write the keys you mean, or drop the key to leave the file to the server")
	}
	if err := checkDeclaredKeys(paperWorldDefaultsDeclared, doc, "paper-world-defaults.yml"); err != nil {
		return "", err
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("paper-world-defaults.yml: %w", err)
	}
	return string(out), nil
}

// checkOverlayFiles refuses an overlay that names a file the flavour does not
// write, since a silently ignored key looks exactly like an applied one.
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

func valueOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

// paperGlobal writes the Velocity block of paper-global.yml over the overlay.
// proxies.velocity.online-mode: true trusts Velocity's forwarded
// authentication; false would give every player an offline-mode UUID and
// detach them from their inventories. A wrong-shaped overlay is refused
// rather than treated as absent. Base, overlay, critical is applied by hand
// because Layer is flat; keep the order the same as Layer's.
func paperGlobal(secret, overlay string) (string, error) {
	doc := map[string]any{}
	if strings.TrimSpace(overlay) != "" {
		if err := yaml.Unmarshal([]byte(overlay), &doc); err != nil {
			return "", fmt.Errorf("paper-global.yml: overlay does not parse as YAML: %w", err)
		}
		// An overlay of "null" or "---" parses to a nil map, and writing into that
		// would panic.
		if doc == nil {
			doc = map[string]any{}
		}
		// Before the shape checks, so a key Paper does not read is reported as such.
		if err := checkDeclaredKeys(paperDeclared, doc, "paper-global.yml"); err != nil {
			return "", err
		}
	}

	// Paper's update checker is off unless the overlay mentions it: the build is
	// pinned by the image, so the call to fill.papermc.io changes nothing and
	// only hits the egress policy on every start. A default, not a critical key.
	if _, set := doc["update-checker"]; !set {
		doc["update-checker"] = map[string]any{"enabled": false}
	}

	proxies := map[string]any{}
	if raw, ok := doc["proxies"]; ok {
		p, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("paper-global.yml: proxies is a %T, want a mapping", raw)
		}
		proxies = p
	}
	velocity := map[string]any{}
	if raw, ok := proxies["velocity"]; ok {
		v, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("paper-global.yml: proxies.velocity is a %T, want a mapping", raw)
		}
		velocity = v
	}

	// Reasserted last. "secret", not "secret-key": Paper ignores keys its
	// GlobalConfiguration.Proxies.Velocity does not declare, leaves secret empty
	// and disables Velocity, so every forwarded join is rejected
	// (TestPaperWritesTheKeysPaperItselfReads checks against Paper's own
	// defaults). PAPER_VELOCITY_SECRET would not keep the secret off disk: Paper
	// writes it into this file itself.
	velocity["enabled"] = true
	velocity["online-mode"] = true
	velocity["secret"] = secret

	proxies["velocity"] = velocity
	doc["proxies"] = proxies

	out, err := yaml.Marshal(doc)
	if err != nil {
		// Unreachable: sigs.k8s.io/yaml unmarshals through JSON, so every value is
		// one yaml.Marshal accepts.
		panic(fmt.Sprintf("paperGlobal: marshalling a known-good document failed: %v", err))
	}
	return string(out), nil
}

// parseProperties skips a line with no '=' rather than failing, so a stray
// line cannot turn a harmless overlay into a crash loop.
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
		out[unescapeProperty(strings.TrimSpace(key))] = unescapeProperty(strings.TrimSpace(value))
	}
	return out
}

// writeProperties sorts the keys so the bytes are stable across renders.
func writeProperties(props map[string]string) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", escapeProperty(k), escapeProperty(props[k]))
	}
	return b.String()
}

// escapeProperty writes java.util.Properties syntax. Without it a MOTD ending
// in a backslash reads as a line continuation and swallows the next key.
// Keys cannot contain '=', so keys and values escape alike.
func escapeProperty(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\f':
			b.WriteString(`\f`)
		case i == 0 && r == ' ':
			// Java strips leading whitespace from a value.
			b.WriteString(`\ `)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// unescapeProperty reads an overlay the way Java would, so a value survives
// the round trip through writeProperties. An unknown escape drops its
// backslash, as Java's does; a trailing backslash is kept, since the parser
// is line-based.
func unescapeProperty(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'f':
			b.WriteByte('\f')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
