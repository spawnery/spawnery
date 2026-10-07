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

package worldsync

import "strings"

const (
	ManifestName = "manifest.json"
	LeaseName    = "lease.json"
	ObjectsDir   = "objects/"
	PacksDir     = "packs/"
	// PackBelow: smaller files travel in the generation's pack. The store
	// bills at least 64 kB per object.
	PackBelow = 64 << 10
)

func join(base, rest string) string {
	if base == "" {
		return rest
	}
	return strings.TrimSuffix(base, "/") + "/" + rest
}

// WorldPrefix is where one world's objects live; world is
// "<namespace>/<group>/<key>".
func WorldPrefix(base, world string) string { return join(base, world) + "/" }

func DeletionPrefix(base string) string { return join(base, ".deletions/") }

func DeletionKey(base, world string) string { return join(base, ".deletions/"+world) }
