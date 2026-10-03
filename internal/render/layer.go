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

// Layer resolves defaults, overlay and critical keys into one flat key set,
// later layers winning. paperGlobal and velocityToml repeat this order by
// hand for their nested documents; change all three together. The inputs
// are not mutated.
func Layer(base, overlay, critical map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overlay)+len(critical))
	for _, source := range []map[string]string{base, overlay, critical} {
		for k, v := range source {
			out[k] = v
		}
	}
	return out
}
