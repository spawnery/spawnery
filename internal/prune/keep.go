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

package prune

import "strings"

// ControlDir sits at the root of /data and is never pruned: the node agent
// may have written "ready" before the entrypoint runs.
const ControlDir = ".spawnery-worldsync"

// Keep is a parsed spec.storage.keep list.
type Keep struct{ pats [][]string }

func ParseKeep(entries []string) (Keep, error) {
	pats, err := parseEntries("spec.storage.keep", entries)
	if err != nil {
		return Keep{}, err
	}
	return Keep{pats: pats}, nil
}

// ParseReplace parses spec.storage.replace into the same matcher.
func ParseReplace(entries []string) (Keep, error) {
	pats, err := parseEntries("spec.storage.replace", entries)
	if err != nil {
		return Keep{}, err
	}
	return Keep{pats: pats}, nil
}

// Holds reports whether rel, slash-separated and relative to /data, is or
// lies below a path an entry matches.
func (k Keep) Holds(rel string) bool {
	return kept(k.pats, strings.Split(rel, "/"))
}

// Toward reports whether rel is a directory that an entry could still match
// below, without being matched itself.
func (k Keep) Toward(rel string) bool {
	segs := strings.Split(rel, "/")
	return !kept(k.pats, segs) && onTheWay(k.pats, nil, segs)
}
