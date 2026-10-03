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
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// Not under /var/run/spawnery, the agent's credential mount, which
// podspec.checkMountCollision guards on its own terms.
const (
	// ConfigDir is where cmd/spawnery-config looks by default.
	ConfigDir  = "/etc/spawnery"
	ValuesFile = "config.yaml"
	// SecretFile comes from a Secret, not the ConfigMap that carries Values.
	SecretFile = "forwarding.secret"
	// OverlayDir holds per-flavour overlay files under the base names Paper or
	// Velocity read.
	OverlayDir = "overlay"
)

// Load reads the Values document, the forwarding secret and the optional
// overlay from dir. It is where a missing or empty file becomes a refusal
// naming it; a missing OverlayDir is fine, a missing or empty SecretFile is
// not.
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
	// Velocity reads the secret file itself via Files.readAllLines joined with
	// "", which drops one trailing line terminator and deletes interior ones,
	// while Paper gets this value verbatim. So strip exactly one trailing
	// terminator (which `kubectl create secret --from-file` usually leaves),
	// then refuse edge whitespace and interior \n or \r, leaving what Velocity
	// itself reads.
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

// loadOverlay uses os.Stat, not os.ReadDir's Lstat-based types: the kubelet
// lays ConfigMap keys down as symlinks into a hidden "..data" directory, so
// filtering on IsRegular would silently return an empty overlay. Followed,
// keys resolve to files and "..data" to a directory, which is skipped. A
// missing dir is fine; any other read error is refused.
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

// WriteAll creates parent directories, since config/ does not exist yet on a
// fresh volume.
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
