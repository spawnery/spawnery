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
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// The read-only mount the operator's rendered configuration arrives on.
//
// Deliberately not under /var/run/spawnery: that is the agent's credential
// mount, and podspec.checkMountCollision guards it with a bidirectional
// nesting check it applies to nothing else. Keeping the two apart keeps that
// rule saying the one thing it exists to say.
const (
	// ConfigDir is where cmd/spawnery-config looks by default; a Server's
	// pod mounts the rendered ConfigMap and Secret here.
	ConfigDir = "/etc/spawnery"
	// ValuesFile is the operator's Values document, in ConfigDir.
	ValuesFile = "config.yaml"
	// SecretFile is the forwarding secret, mounted from a Kubernetes Secret
	// rather than carried in the ConfigMap Values comes from.
	SecretFile = "forwarding.secret"
	// OverlayDir holds the operator's optional per-flavour overlay files,
	// keyed by the base name Paper or Velocity reads them under.
	OverlayDir = "overlay"
)

// Load reads the three inputs a flavour needs off ConfigDir: the Values
// document, the forwarding secret, and an optional overlay.
//
// It is the one place "file missing" or "file empty" turns into a refusal
// naming the file and the key, so that Paper and Velocity can assume their
// three arguments already exist and need only be validated for content. A
// missing OverlayDir is not a refusal — the overlay is optional — but a
// missing or empty SecretFile is: Paper and Velocity both refuse an empty
// secret already, but only because Load hands them "" rather than never
// getting that far, and a caller that skipped Load could not tell "the
// secret is empty" apart from "the secret was never read".
func Load(dir string) (Values, string, map[string]string, error) {
	valuesPath := filepath.Join(dir, ValuesFile)
	data, err := os.ReadFile(valuesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Values{}, "", nil, fmt.Errorf("%s: not found", valuesPath)
		}
		return Values{}, "", nil, fmt.Errorf("%s: %w", valuesPath, err)
	}
	var v Values
	if err := yaml.Unmarshal(data, &v); err != nil {
		return Values{}, "", nil, fmt.Errorf("%s: does not parse as the Values document: %w", valuesPath, err)
	}

	secretPath := filepath.Join(dir, SecretFile)
	secretData, err := os.ReadFile(secretPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Values{}, "", nil, fmt.Errorf("%s: not found", secretPath)
		}
		return Values{}, "", nil, fmt.Errorf("%s: %w", secretPath, err)
	}
	// Canonicalise, then refuse. Paper gets this package's own return value
	// written verbatim into paper-global.yml, but Velocity is handed
	// secretPath and reads the file itself — and the pinned 3.5.1-615 jar
	// joins the file's lines with Files.readAllLines and "", which drops
	// exactly one trailing \n or \r\n (readAllLines never emits a further
	// empty line for it) and would delete an internal one too. A single
	// trailing line terminator is therefore not a divergence — both this
	// function's TrimSpace-based history and Velocity's own read have always
	// discarded it the same way — but treating a raw byte comparison as the
	// refusal predicate did, and that refused every operator-authored Secret
	// this repository's own config/samples/network.yaml documents producing
	// (`head -c 32 /dev/urandom | base64`, which `kubectl create secret
	// generic --from-file` carries into the mount terminator and all).
	//
	// So: strip exactly one trailing terminator first, then apply the actual
	// refusal — edge whitespace (Velocity's read would keep it, Paper's
	// write already has it, so it never disagrees on its own, but it is
	// exactly the shape an operator-authored Secret should never need) and
	// any interior \n or \r (Velocity's read deletes it, Paper's does not,
	// which is a real divergence). What is left after that strip is what
	// Velocity's own read produces from the raw file, so returning it here
	// is what keeps Paper's copy and Velocity's read of the same Secret
	// equal.
	secret := string(secretData)
	canonical := secret
	switch {
	case strings.HasSuffix(canonical, "\r\n"):
		canonical = canonical[:len(canonical)-2]
	case strings.HasSuffix(canonical, "\n"):
		canonical = canonical[:len(canonical)-1]
	}
	if strings.TrimSpace(canonical) == "" {
		return Values{}, "", nil, fmt.Errorf("%s: forwarding secret is empty", secretPath)
	}
	if strings.TrimSpace(canonical) != canonical {
		return Values{}, "", nil, fmt.Errorf(
			"%s: forwarding secret must not carry surrounding whitespace", secretPath)
	}
	if strings.ContainsAny(canonical, "\n\r") {
		return Values{}, "", nil, fmt.Errorf(
			"%s: forwarding secret must not contain an interior line break", secretPath)
	}
	secret = canonical

	overlay, err := loadOverlay(filepath.Join(dir, OverlayDir))
	if err != nil {
		return Values{}, "", nil, err
	}

	return v, secret, overlay, nil
}

// loadOverlay reads every file directly under dir into a map keyed by base
// name — the same bare names Paper's and Velocity's overlay keys use, since
// a ConfigMap key cannot contain '/'.
//
// dir is not read with os.ReadDir's own Lstat-based entry types, because a
// ConfigMap or Secret mounted as a directory — the idiom internal/podspec
// already uses for the agent CA and for a Server's own mounts — does not
// present its keys as regular files. The kubelet lays each key down as a
// symlink into a hidden "..data" directory, so it can swap that one symlink
// atomically on update; "..data" itself is a symlink to a further hidden,
// timestamped directory holding the real content. Lstat sees the key
// symlinks as symlinks, not regular files, so filtering on
// DirEntry.Type().IsRegular() — what os.ReadDir's entries report — skips
// every one of them and returns an empty overlay with no error: the exact
// failure this package exists to prevent, a configuration that silently did
// not take effect. os.Stat, used here instead, follows symlinks: a key
// resolves to the real file and is read, while "..data" and the timestamped
// directory it points at both resolve to directories and are skipped, with
// no special case needed for Kubernetes' naming convention.
//
// A missing dir is not an error: the overlay is optional, and most Servers
// will never render one. Anything else that stops a read — a permission bit
// this container's user does not have, most plausibly — is, because a
// silently-dropped overlay entry is indistinguishable from one that took
// effect.
func loadOverlay(dir string) (map[string]string, error) {
	overlay := map[string]string{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return overlay, nil
		}
		return nil, fmt.Errorf("%s: %w", dir, err)
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if info.IsDir() {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		overlay[entry.Name()] = string(content)
	}
	return overlay, nil
}

// WriteAll writes every rendered file under root, creating parent
// directories first so a key like "config/paper-global.yml" lands even
// though config/ does not exist yet on a fresh volume.
func WriteAll(root string, files map[string][]byte) error {
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}
