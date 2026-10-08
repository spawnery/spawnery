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
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func observations(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

func wantWorlds(t *testing.T, mounted, cached float64) {
	t.Helper()
	m, c := testutil.ToFloat64(worlds.WithLabelValues("mounted")), testutil.ToFloat64(worlds.WithLabelValues("cached"))
	if m != mounted || c != cached {
		t.Fatalf("worlds mounted = %v, cached = %v; want %v and %v", m, c, mounted, cached)
	}
}

func TestTheMetricsFollowAWorldFromNodeToNode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	uploads, downloads, failures := observations(t, uploadSeconds), observations(t, downloadSeconds), testutil.ToFloat64(downloadFailures)

	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.node("a").kickUploads(ctx)
	wantWorlds(t, 1, 0)

	h.unpublish("a", target)
	h.node("a").Settle(ctx)
	wantWorlds(t, 0, 1)
	if got := observations(t, uploadSeconds); got != uploads+1 {
		t.Fatalf("upload observations = %d, want %d", got, uploads+1)
	}

	target, _ = h.publish("b", w, "p2")
	h.waitFile(target, ReadyFile)
	if got := observations(t, downloadSeconds); got != downloads+1 {
		t.Fatalf("download observations = %d, want %d", got, downloads+1)
	}

	h.unpublish("b", target)
	h.node("b").Settle(ctx)
	packs, err := h.st.List(ctx, WorldPrefix("", w)+PacksDir)
	if err != nil || len(packs) == 0 {
		t.Fatalf("packs = %v, %v", packs, err)
	}
	if err := h.st.Delete(ctx, packs[0]); err != nil {
		t.Fatal(err)
	}
	target, _ = h.publish("c", w, "p3")
	h.waitFile(target, FailedFile)
	if got := testutil.ToFloat64(downloadFailures); got != failures+1 {
		t.Fatalf("download failures = %v, want %v", got, failures+1)
	}
}
