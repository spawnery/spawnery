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
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"sigs.k8s.io/yaml"
)

// defaultsDir holds the configuration each receiving program writes for
// itself, byte for byte, out of the jar this repository pins.
//
// These were testdata until 2026-08-24 and are not any more: the renderer
// reads them at startup, to refuse an overlay key the program on the other end
// does not declare. Deleting them as unused fixtures would take the check with
// them, which is why they no longer live under a name that says "tests only".
// paper_test.go and velocity_test.go carry the commands that regenerate each.
const defaultsDir = "defaults"

//go:embed defaults/paper-global.default.yml
var paperDefaultConfig []byte

//go:embed defaults/velocity.default.toml
var velocityDefaultConfig []byte

// keyNode is one level of a configuration document's declared shape.
//
// The shape is measured, not written down: it comes from the receiving
// program's own default file, so a Paper or Velocity bump moves it by moving
// that file. That is the trade this check makes, taken deliberately on
// 2026-08-24 — a bump refuses a legitimate override for a newly added key
// until the file is regenerated, and in exchange a key the program does not
// read stops passing silently. The refusal happens at render time and names
// the key; the silence happened in a cluster and named nothing, twice.
type keyNode struct {
	// children are the keys declared at this level.
	children map[string]*keyNode
	// freeForm marks a level whose child names belong to whoever writes the
	// configuration rather than to the program: Velocity's [servers] is keyed
	// by server names somebody chose, Paper's packet-limiter.overrides by
	// packet ids. children still holds the reserved names declared at such a
	// level (servers.try is Velocity's, not a server), and shape is what every
	// other child is checked against.
	freeForm bool
	shape    *keyNode
}

// freeFormPath names a level whose child names are the user's, and the names
// at that level that are still the program's.
//
// This is knowledge about the receiving program that its default file cannot
// carry: nothing in the document distinguishes "lobby, an example server
// somebody may replace" from "try, a key Velocity reads". Both are keys under
// [servers].
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

var velocityFreeForm = []freeFormPath{
	// Keyed by server name. try is Velocity's own reserved key in there, and
	// the fixture's lobby/factions/minigames are its three example servers.
	{path: "servers", reserved: []string{"try"}},
	// Keyed by hostname.
	{path: "forced-hosts"},
}

// The two trees are built once at startup. A malformed default file is a
// broken build rather than a runtime error: these are checked-in files this
// repository measured itself, not input.
var (
	paperDeclared    = mustKeyTree(paperDefaultConfig, unmarshalYAML, paperFreeForm, "paper-global.yml")
	velocityDeclared = mustKeyTree(velocityDefaultConfig, unmarshalTOML, velocityFreeForm, "velocity.toml")
)

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

// buildKeyNode turns one level of a parsed default document into a keyNode.
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
	// Deterministic, and the choice is only ever between children of the same
	// shape: TestEveryFreeFormExampleHasTheSameShape asserts that, so this
	// picks one rather than trusting one.
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
// not declare.
//
// The program does not refuse it — that is the whole problem. Paper keeps its
// own default for the field the author meant and writes the stray key straight
// back out on the next save, so the document on disk goes on looking like the
// override took; Velocity's night-config reads out the keys it asks for and a
// misspelling is a key nobody reads. Two outages came through this door:
// proxies.velocity.secret-key for secret (milestone 3c, every forwarded join
// refused) and haproxy-protocol at the top level instead of under [advanced]
// (the RKE2 rollout, half a day spent suspecting the reverse proxy).
//
// That second one is what establishes "silently ignored" as a measurement
// rather than a reading of the code. Driven 2026-08-20 against a scratch
// ProxyGroup with a hand-built PROXY v1 header sent straight to the pod, no
// reverse proxy involved:
//
//	key placed          no header          with header
//	top level           status response    silence
//	under [advanced]    silence            status response
//
// So the misplaced key leaves Velocity behaving exactly as if the setting were
// absent, and the rendered file reads exactly as the author intended. Nothing
// downstream distinguishes the two until a connection behaves strangely, which
// is the cost this refusal buys out.
func checkDeclaredKeys(declared *keyNode, overlay map[string]any, what string) error {
	return walkOverlay(declared, declared, overlay, "", what)
}

func walkOverlay(root, node *keyNode, level map[string]any, path, what string) error {
	// Sorted, so a document with two undeclared keys always reports the same
	// one and a failure is reproducible.
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

// undeclared builds the refusal. It says where the key *is* declared when it
// is declared somewhere else, because that is the shape both of this project's
// outages took: a real key at the wrong depth, not an invented one.
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

// declaredAt finds every path at which a key of this name is declared.
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
