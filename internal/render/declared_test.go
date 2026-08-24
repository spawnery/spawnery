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
	"sort"
	"strings"
	"testing"
)

// buildKeyNode picks one of a free-form level's example children as the shape
// every user-chosen name is checked against. That is only sound if the
// examples agree, and nothing but this says they do — a fixture where
// packet-limiter.overrides gained a second entry with different sub-keys would
// make the check depend on which one sorted first.
func TestEveryFreeFormExampleHasTheSameShape(t *testing.T) {
	for _, tc := range []struct {
		what     string
		doc      []byte
		parse    func([]byte) (map[string]any, error)
		freeForm []freeFormPath
	}{
		{"paper-global.yml", paperDefaultConfig, unmarshalYAML, paperFreeForm},
		{"velocity.toml", velocityDefaultConfig, unmarshalTOML, velocityFreeForm},
	} {
		t.Run(tc.what, func(t *testing.T) {
			parsed, err := tc.parse(tc.doc)
			if err != nil {
				t.Fatalf("%s does not parse: %v", tc.what, err)
			}
			// Built without the free-form list, so every example child is
			// still present to be compared.
			full := buildKeyNode(parsed, nil, "")
			for _, free := range tc.freeForm {
				node := nodeAt(full, free.path)
				if node == nil {
					t.Fatalf("%s declares no %s", tc.what, free.path)
				}
				var shapes []string
				var names []string
				for name, child := range node.children {
					if contains(free.reserved, name) {
						continue
					}
					names = append(names, name)
					shapes = append(shapes, shapeOf(child))
				}
				if len(names) == 0 {
					t.Fatalf("%s's %s has no example child at all; there is nothing to "+
						"check a user-chosen name against", tc.what, free.path)
				}
				for i := range shapes {
					if shapes[i] != shapes[0] {
						t.Errorf("%s's %s: example %q has shape %s, %q has %s. buildKeyNode "+
							"picks one of them by sort order, so the check would depend on "+
							"which", tc.what, free.path, names[i], shapes[i], names[0], shapes[0])
					}
				}
			}
		})
	}
}

// Every free-form path has to name a level the fixture actually has, or the
// check silently measures user-chosen names against a schema. mustKeyTree
// panics on that at package load, which makes it a build failure rather than a
// test — this asserts the same thing on the tree that was built, so the
// failure reads as a sentence rather than as a panic in every test in the
// package.
func TestEveryFreeFormPathExists(t *testing.T) {
	for _, tc := range []struct {
		what     string
		tree     *keyNode
		freeForm []freeFormPath
	}{
		{"paper-global.yml", paperDeclared, paperFreeForm},
		{"velocity.toml", velocityDeclared, velocityFreeForm},
	} {
		for _, free := range tc.freeForm {
			node := nodeAt(tc.tree, free.path)
			if node == nil {
				t.Errorf("%s: %s is listed as free-form but the tree has no such level",
					tc.what, free.path)
				continue
			}
			if !node.freeForm {
				t.Errorf("%s: %s is listed as free-form but the tree did not mark it",
					tc.what, free.path)
			}
		}
	}
}

// shapeOf renders a node's key structure, so two example children can be
// compared as strings.
func shapeOf(node *keyNode) string {
	if node == nil || len(node.children) == 0 {
		return "{}"
	}
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+shapeOf(node.children[name]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
