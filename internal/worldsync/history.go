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

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HistoryKey carries what retention and a listing need, so that neither
// reads the entry.
func HistoryKey(generation int64, taken time.Time) string {
	return fmt.Sprintf("%s%020d-%d.json", HistoryDir, generation, taken.UnixMilli())
}

func ParseHistoryKey(rel string) (Point, bool) {
	name, ok := strings.CutPrefix(rel, HistoryDir)
	if !ok {
		return Point{}, false
	}
	name, ok = strings.CutSuffix(name, ".json")
	if !ok {
		return Point{}, false
	}
	gen, ms, ok := strings.Cut(name, "-")
	if !ok || len(gen) != 20 {
		return Point{}, false
	}
	g, err := strconv.ParseInt(gen, 10, 64)
	if err != nil || g <= 0 {
		return Point{}, false
	}
	t, err := strconv.ParseInt(ms, 10, 64)
	if err != nil || t < 0 {
		return Point{}, false
	}
	return Point{Generation: g, Taken: time.UnixMilli(t).UTC()}, true
}

type HistoryEntry struct {
	Point
	Key string
}

func ListHistory(ctx context.Context, st Store, prefix string) ([]HistoryEntry, error) {
	keys, err := st.List(ctx, prefix+HistoryDir)
	if err != nil {
		return nil, err
	}
	var out []HistoryEntry
	for _, k := range keys {
		if p, ok := ParseHistoryKey(strings.TrimPrefix(k, prefix)); ok {
			out = append(out, HistoryEntry{Point: p, Key: k})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Generation > out[j].Generation })
	return out, nil
}

func readHistoryEntry(ctx context.Context, st Store, key string) (Manifest, error) {
	b, _, err := readObject(ctx, st, key)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", key, err)
	}
	return m, nil
}
