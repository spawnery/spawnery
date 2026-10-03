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
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"sigs.k8s.io/yaml"
)

// defaultsDir holds each receiving program's own default configuration, byte
// for byte from the pinned jar. Not test data: the renderer reads it at
// startup. paper_test.go and velocity_test.go carry the regeneration commands.
const defaultsDir = "defaults"

//go:embed defaults/paper-global.default.yml
var paperDefaultConfig []byte

//go:embed defaults/paper-world-defaults.default.yml
var paperWorldDefaultsDefaultConfig []byte

//go:embed defaults/velocity.default.toml
var velocityDefaultConfig []byte

//go:embed defaults/server.properties.default
var paperPropertiesDefaultConfig []byte

// keyNode is one level of a configuration document's declared shape, taken
// from the program's own default file. A version bump refuses overrides of
// newly added keys until the file is regenerated, in exchange for refusing
// keys the program does not read.
type keyNode struct {
	children map[string]*keyNode
	// freeForm marks a level whose child names are the user's (Velocity's
	// [servers], Paper's packet-limiter.overrides). children still holds the
	// reserved names at such a level; every other child is checked against
	// shape.
	freeForm bool
	shape    *keyNode
}

// freeFormPath is knowledge the default file cannot carry: nothing in it
// distinguishes the example server "lobby" from Velocity's own key "try".
type freeFormPath struct {
	path     string
	reserved []string
}

var paperFreeForm = []freeFormPath{
	// Keyed by item id; the example entry is minecraft:elytra.
	{path: "anticheat.obfuscation.items.model-overrides"},
	// Keyed by packet id; the example entry is minecraft:place_recipe.
	{path: "packet-limiter.overrides"},
}

// paper-world-defaults.yml declares none although tick-rates.behavior and
// tick-rates.sensor are user-keyed: they nest a free-form level inside
// another, which buildKeyNode does not support. Overrides there for any
// entity but villager are refused, loudly.
var paperWorldDefaultsFreeForm []freeFormPath

var velocityFreeForm = []freeFormPath{
	// Keyed by server name; the fixture's lobby/factions/minigames are examples.
	{path: "servers", reserved: []string{"try"}},
	// Keyed by hostname.
	{path: "forced-hosts"},
}

// Built at startup; a malformed default file is a broken build, not input.
var (
	paperDeclared = mustKeyTree(paperDefaultConfig, unmarshalYAML, paperFreeForm, "paper-global.yml")
	// Purpur's copy of this file is byte-identical to Paper's, so one tree
	// serves both images.
	paperWorldDefaultsDeclared = mustKeyTree(
		paperWorldDefaultsDefaultConfig, unmarshalYAML, paperWorldDefaultsFreeForm,
		"paper-world-defaults.yml")
	velocityDeclared        = mustKeyTree(velocityDefaultConfig, unmarshalTOML, velocityFreeForm, "velocity.toml")
	paperPropertiesDeclared = mustKeyTree(
		paperPropertiesDefaultConfig, unmarshalProperties, nil, "server.properties")
)

// unmarshalProperties delegates to parseProperties, the parser applied to a
// user's overlay, so the check and the renderer agree on what a key is.
func unmarshalProperties(doc []byte) (map[string]any, error) {
	flat := parseProperties(string(doc))
	out := make(map[string]any, len(flat))
	for k, v := range flat {
		out[k] = v
	}
	return out, nil
}

func unmarshalYAML(doc []byte) (map[string]any, error) {
	var out map[string]any
	err := yaml.Unmarshal(doc, &out)
	return out, err
}

func unmarshalTOML(doc []byte) (map[string]any, error) {
	var out map[string]any
	err := toml.Unmarshal(doc, &out)
	return out, err
}

func mustKeyTree(doc []byte, parse func([]byte) (map[string]any, error),
	freeForm []freeFormPath, what string) *keyNode {
	parsed, err := parse(doc)
	if err != nil {
		panic(fmt.Sprintf("render: %s's own defaults do not parse: %v", what, err))
	}
	if len(parsed) == 0 {
		panic(fmt.Sprintf("render: %s's own defaults have no keys at all", what))
	}
	free := make(map[string][]string, len(freeForm))
	for _, f := range freeForm {
		free[f.path] = f.reserved
	}
	tree := buildKeyNode(parsed, free, "")
	for path := range free {
		if nodeAt(tree, path) == nil {
			panic(fmt.Sprintf("render: %s declares no %s, but it is listed as free-form; "+
				"the default file moved under the list", what, path))
		}
	}
	return tree
}

func buildKeyNode(level map[string]any, free map[string][]string, path string) *keyNode {
	node := &keyNode{children: make(map[string]*keyNode, len(level))}
	reserved, isFree := free[path]
	for name, value := range level {
		child := &keyNode{}
		if nested, ok := value.(map[string]any); ok {
			child = buildKeyNode(nested, free, join(path, name))
		}
		node.children[name] = child
	}
	if !isFree {
		return node
	}
	node.freeForm = true
	keep := make(map[string]*keyNode, len(reserved))
	var examples []string
	for name := range node.children {
		if contains(reserved, name) {
			keep[name] = node.children[name]
			continue
		}
		examples = append(examples, name)
	}
	// TestEveryFreeFormExampleHasTheSameShape guarantees the examples agree, so
	// any deterministic pick will do.
	sort.Strings(examples)
	if len(examples) > 0 {
		node.shape = node.children[examples[0]]
	} else {
		node.shape = &keyNode{}
	}
	node.children = keep
	return node
}

func nodeAt(root *keyNode, path string) *keyNode {
	node := root
	for _, seg := range strings.Split(path, ".") {
		if node == nil {
			return nil
		}
		node = node.children[seg]
	}
	return node
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// checkDeclaredKeys refuses the first overlay key the receiving program does
// not declare, because neither program does: Paper keeps its default and
// writes the stray key back out, Velocity's night-config never reads it, and
// the rendered file looks exactly as intended.
func checkDeclaredKeys(declared *keyNode, overlay map[string]any, what string) error {
	return walkOverlay(declared, declared, overlay, "", what)
}

func walkOverlay(root, node *keyNode, level map[string]any, path, what string) error {
	// Sorted, so the reported key is reproducible.
	names := make([]string, 0, len(level))
	for name := range level {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		child, ok := lookup(node, name)
		if !ok {
			return undeclared(root, path, name, node, what)
		}
		nested, isMap := level[name].(map[string]any)
		if !isMap {
			continue
		}
		if err := walkOverlay(root, child, nested, join(path, name), what); err != nil {
			return err
		}
	}
	return nil
}

func lookup(node *keyNode, name string) (*keyNode, bool) {
	if child, ok := node.children[name]; ok {
		return child, true
	}
	if node.freeForm {
		return node.shape, true
	}
	return nil, false
}

// undeclared names where the key is declared if it exists elsewhere: a real
// key at the wrong depth is the common mistake.
func undeclared(root *keyNode, path, name string, node *keyNode, what string) error {
	where := "the top level"
	if path != "" {
		where = path
	}
	if elsewhere := declaredAt(root, name, ""); len(elsewhere) > 0 {
		return fmt.Errorf("%s: the overlay sets %q under %s, which does not declare it; "+
			"it is declared at %s. A key at the wrong depth is not read and not refused — "+
			"the rendered file looks right and the setting never applies",
			what, name, where, strings.Join(elsewhere, ", "))
	}
	return fmt.Errorf("%s: the overlay sets %q under %s, which declares %s. "+
		"An unknown key is kept in the file and ignored by the program, so the override "+
		"would silently do nothing",
		what, name, where, list(node))
}

func declaredAt(node *keyNode, name, path string) []string {
	var found []string
	for child, sub := range node.children {
		if child == name {
			found = append(found, join(path, child))
		}
		found = append(found, declaredAt(sub, name, join(path, child))...)
	}
	if node.shape != nil {
		found = append(found, declaredAt(node.shape, name, join(path, "*"))...)
	}
	sort.Strings(found)
	return found
}

func list(node *keyNode) string {
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Strings(names)
	if node.freeForm {
		if len(names) == 0 {
			return "names of its own"
		}
		return "names of its own, plus " + strings.Join(names, ", ")
	}
	if len(names) == 0 {
		return "no keys at all — it is not a table"
	}
	return strings.Join(names, ", ")
}
