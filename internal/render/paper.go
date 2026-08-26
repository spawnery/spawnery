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
// The four fields in server.properties that the operator relies on are in the
// critical layer and no overlay can move them:
//
//   - server-port, because internal/podspec names 25565 and a pod whose
//     process listens elsewhere passes no probe;
//   - online-mode=false, because the proxy authenticates players and forwards
//     the result — with it on, modern forwarding fails every join;
//   - enable-status, because with it off the server answers no server list
//     ping, the readiness probe stays red forever, and nothing in the log says
//     why;
//   - enforce-secure-profile=false, because Paper's own default of true
//     refuses any join lacking a Mojang-signed chat session, which
//     online-mode=false above means no join ever has — left at the default,
//     every join would fail; turned off, a backend reached directly instead
//     of through the proxy accepts unsigned chat from an unauthenticated
//     connection too. Nothing in this repository closes that off: there is
//     no NetworkPolicy anywhere under config/, so today any pod in the
//     cluster that can reach port 25565 can attempt that connection. See
//     "A NetworkPolicy restricting backends to proxies-only is now overdue"
//     in docs/known-issues.md for why that is a real exposure rather than a
//     formality, and which milestone owns closing it.
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

	// Before the layering, so a key Minecraft does not read is reported as
	// the key it is rather than silently added to the file and ignored.
	//
	// This was the one overlay nothing checked. paper-global.yml and
	// velocity.toml have been measured against their programs' own defaults
	// since 2026-08-24; server.properties was left out because its default
	// file is Minecraft's and this repository had never captured one. It has
	// now -- defaults/server.properties.default, written by the pinned build
	// on a first start, the same way the other two were -- so the same rule
	// applies to all three.
	//
	// What it catches is narrow and worth stating: the four keys the operator
	// relies on are in the critical layer below and no overlay can move them,
	// so a typo could only ever reach the author's own settings. It reached
	// them silently, which is the part that changed.
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
// Layer: Layer is typed map[string]string, and paper-global.yml is a nested
// document that would need flattening to dotted keys and re-nesting to use
// it, which the plan declined in favour of duplicating the three-step order
// here. The two implementations must be kept in the same order — see the
// note on Layer.
//
// The overlay is the base document. It used to be read for its
// proxies.velocity keys and nothing else: the rendered file was built from
// scratch as {proxies: {velocity: ...}}, so every other key an overlay set —
// every part of paper-global.yml that is not the Velocity block — was parsed,
// dropped, and never written, while paperOverlayKeys advertised the file as
// one an overlay may set and checkOverlayFiles' own comment called a silently
// dropped overlay key worse than an error. Nothing said so anywhere, and no
// test looked outside proxies.velocity, so an override for, say,
// chunk-loading.autoconfig-send-distance rendered a file that did not contain
// it and a server that came up looking healthy.
func paperGlobal(secret, overlay string) (string, error) {
	doc := map[string]any{}
	if strings.TrimSpace(overlay) != "" {
		if err := yaml.Unmarshal([]byte(overlay), &doc); err != nil {
			return "", fmt.Errorf("paper-global.yml: overlay does not parse as YAML: %w", err)
		}
		// An overlay of "null" or "---" parses to a nil map rather than an
		// error, and writing into that would panic.
		if doc == nil {
			doc = map[string]any{}
		}
		// Before the shape checks below, so a key Paper does not read is
		// reported as the key it is rather than as whatever the shape check
		// makes of it. See checkDeclaredKeys for the trade this takes.
		if err := checkDeclaredKeys(paperDeclared, doc, "paper-global.yml"); err != nil {
			return "", err
		}
	}

	// Paper's update checker, off unless the user asked for it.
	//
	// It is an outbound call to fill.papermc.io on every start, and in a fleet
	// whose Paper build is pinned by nix/paper.nix it can change nothing: the
	// answer reaches a log line nobody acts on, because upgrading means a new
	// image and not a running server downloading anything. What it does reach
	// is the egress policy. Milestone 6b's per-Network policy already permits
	// no general egress, so on a cluster whose CNI enforces it this call
	// already fails; leaving the default on means every server start spends a
	// DNS lookup and a connect attempt on a request that is designed to be
	// refused, and means anybody tightening egress has to decide about a
	// dependency the operator does not need.
	//
	// A default and not a critical key. It is set only where the overlay has
	// said nothing about update-checker at all, so a user who wants it back
	// writes `update-checker: {enabled: true}` and gets it -- unlike the
	// velocity block below, which is reasserted whatever the overlay said,
	// because those keys decide whether a join works.
	if _, set := doc["update-checker"]; !set {
		doc["update-checker"] = map[string]any{"enabled": false}
	}

	// The two shapes below are refused rather than treated as an absent
	// overlay, for the reason above the function: an overlay that silently
	// does nothing looks exactly like one that took effect.
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

	// Reasserted last: whatever the overlay said about these three keys is
	// overwritten rather than merged around.
	//
	// "secret", not "secret-key". Paper ignores a key its own
	// GlobalConfiguration.Proxies.Velocity does not declare — it does not
	// refuse the file, and it writes the stray key straight back out on the
	// next save, so the document on disk keeps looking like the override took.
	// Meanwhile secret stays at its default of '', and Paper's own postProcess
	// turns enabled back off ("Velocity is enabled, but no secret key was
	// specified. A secret key is required. Disabling velocity..."), leaving a
	// backend that starts cleanly, passes every probe, and rejects every
	// forwarded join. This spelling was wrong from the day it was written until
	// milestone 3c's first end-to-end join found it; see
	// TestPaperWritesTheKeysPaperItselfReads for the check that now measures
	// these names against Paper's own defaults rather than against this file.
	// The environment is not a way to keep this secret off disk, and it looks
	// like one. Paper 26.2 does read PAPER_VELOCITY_SECRET -- measured
	//2026-08-26 against the pinned build, booted with a paper-global.yml
	// carrying enabled and online-mode and no secret at all: forwarding came
	// up, with none of the "no secret key was specified. Disabling velocity"
	// that a genuinely missing secret produces.
	//
	// And then Paper wrote it into config/paper-global.yml itself:
	//
	//	velocity:
	//	  enabled: true
	//	  online-mode: true
	//	  secret: measured-from-the-environment
	//
	// So the plaintext reaches the same file either way, and for a persistent
	// group that file is on the PersistentVolume. What the environment would
	// add is a second copy -- in the pod spec, unless it came by secretKeyRef
	// -- for no reduction anywhere. docs/known-issues.md carried this as a
	// smaller attack surface waiting to be taken; it is not one.
	velocity["enabled"] = true
	velocity["online-mode"] = true
	velocity["secret"] = secret

	proxies["velocity"] = velocity
	doc["proxies"] = proxies

	out, err := yaml.Marshal(doc)
	if err != nil {
		// Nothing reaches this branch. The document is the overlay's own
		// value tree with three keys asserted over it, and sigs.k8s.io/yaml
		// unmarshals through JSON — so every value in it is a string, bool,
		// float64, nil, []any or map[string]any, and yaml.Marshal accepts all
		// of them. A user cannot smuggle an unmarshalable value in here.
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
