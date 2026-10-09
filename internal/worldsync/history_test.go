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
	"strings"
	"testing"
	"time"
)

func TestAHistoryKeyCarriesGenerationAndTimeAndSortsByGeneration(t *testing.T) {
	taken := time.Date(2026, 10, 8, 10, 0, 0, 123_000_000, time.UTC)
	k := HistoryKey(7, taken)
	if !strings.HasPrefix(k, "history/00000000000000000007-") || !strings.HasSuffix(k, ".json") {
		t.Fatalf("key = %q", k)
	}
	p, ok := ParseHistoryKey(k)
	if !ok || p.Generation != 7 || !p.Taken.Equal(taken) {
		t.Fatalf("ParseHistoryKey(%q) = %+v, %v", k, p, ok)
	}
	if HistoryKey(9, taken) >= HistoryKey(10, taken) {
		t.Fatal("keys of generations 9 and 10 do not sort by generation")
	}
}

func TestParseHistoryKeyRefusesWhatIsNoEntry(t *testing.T) {
	for _, k := range []string{
		"history/7-1.json",
		"history/x.json",
		"history/00000000000000000007-1.tmp",
		"history/00000000000000000007--5.json",
		"history/00000000000000000000-1.json",
		"objects/00000000000000000007-1.json",
	} {
		if p, ok := ParseHistoryKey(k); ok {
			t.Errorf("ParseHistoryKey(%q) = %+v, want refused", k, p)
		}
	}
}

func TestListHistoryIsNewestFirstAndSkipsStrangers(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	taken := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	for _, k := range []string{HistoryKey(2, taken), HistoryKey(10, taken), HistoryKey(1, taken), "history/README"} {
		if _, err := st.Put(ctx, prefix+k, strings.NewReader("{}"), PutCondition{}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil {
		t.Fatal(err)
	}
	var gens []int64
	for _, e := range entries {
		gens = append(gens, e.Generation)
		if !strings.HasPrefix(e.Key, prefix+HistoryDir) {
			t.Errorf("key %q is not the full key", e.Key)
		}
	}
	if len(gens) != 3 || gens[0] != 10 || gens[1] != 2 || gens[2] != 1 {
		t.Fatalf("generations = %v, want [10 2 1]", gens)
	}
}

func TestTakenAtFallsBackToWhenTheManifestWasWritten(t *testing.T) {
	written := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if got := (Manifest{}).TakenAt(written); !got.Equal(written) {
		t.Fatalf("TakenAt of a manifest without taken = %v, want %v", got, written)
	}
	taken := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if got := (Manifest{Taken: taken.UnixMilli()}).TakenAt(written); !got.Equal(taken) {
		t.Fatalf("TakenAt = %v, want %v", got, taken)
	}
}
