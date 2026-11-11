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

package leaderonly

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func newGauge() prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "leaderonly_test_gauge", Help: "h"})
	g.Set(42)
	return g
}

func names(t *testing.T, r *prometheus.Registry) map[string]float64 {
	t.Helper()
	fams, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range fams {
		out[f.GetName()] = f.GetMetric()[0].GetGauge().GetValue()
	}
	return out
}

func TestAWrappedCollectorIsSilentUntilElected(t *testing.T) {
	reset()
	r := prometheus.NewRegistry()
	r.MustRegister(Wrap(newGauge()))
	if got := names(t, r); len(got) != 0 {
		t.Fatalf("standby gathered %v", got)
	}
	Elect()
	if got := names(t, r); got["leaderonly_test_gauge"] != 42 {
		t.Fatalf("leader gathered %v", got)
	}
}

func TestAWrappedCollectorStillDescribes(t *testing.T) {
	reset()
	g := newGauge()
	ch := make(chan *prometheus.Desc, 4)
	Wrap(g).Describe(ch)
	close(ch)
	d := <-ch
	if d == nil || d.String() != g.Desc().String() {
		t.Fatalf("Describe yielded %v, want %v", d, g.Desc())
	}
}

func TestWatchElectsWhenTheChannelCloses(t *testing.T) {
	reset()
	r := prometheus.NewRegistry()
	r.MustRegister(Wrap(newGauge()))
	elected := make(chan struct{})
	Watch(elected)
	close(elected)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(names(t, r)) == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("gauge did not appear within 1s of the channel closing")
}
