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

// Layer resolves the three configuration sources into one flat key set.
//
// The order is the contract that section 3 of the design fixes spells out,
// and is the reason this is a function rather than three assignments spread
// through two flavours: rendered defaults lose to the user's overlay, and
// both lose to the fields an operator must not be able to break. A flavour
// that applied them in its own order would be a second answer to a question
// that has one.
//
// Every target format reduces to a flat key set before it is serialised —
// dotted keys for the nested ones — so one merge serves all three files.
//
// Two exceptions, both deliberate calls rather than oversights — see the
// note on each:
//
//   - internal/render/paper.go's paperGlobal reimplements this same
//     base-then-overlay-then-critical order by hand for paper-global.yml's
//     nested proxies.velocity block, rather than flattening it to dotted
//     keys and nesting on write.
//   - internal/render/velocity.go's velocityToml does the same for
//     velocity.toml, whose [servers] table is likewise nested.
//
// This order and both of theirs must be changed together, or the three
// files would silently disagree about which layer wins.
//
// The inputs are not mutated: callers hold them for the next file.
func Layer(base, overlay, critical map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overlay)+len(critical))
	for _, source := range []map[string]string{base, overlay, critical} {
		for k, v := range source {
			out[k] = v
		}
	}
	return out
}
