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
	secret := strings.TrimSpace(string(secretData))
	if secret == "" {
		return Values{}, "", nil, fmt.Errorf("%s: forwarding secret is empty", secretPath)
	}

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
