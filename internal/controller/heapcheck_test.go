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

package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func TestHeapMismatch(t *testing.T) {
	gi := func(s string) *resource.Quantity { q := resource.MustParse(s); return &q }
	const mib = 1 << 20
	cases := []struct {
		name     string
		want     *resource.Quantity
		reported int64
		mismatch bool
	}{
		{"as asked", gi("5Gi"), 5120 * mib, false},
		{"within tolerance", gi("5Gi"), 4915 * mib, false},
		{"above tolerance", gi("5Gi"), 5400 * mib, true},
		{"below tolerance", gi("5Gi"), 4608 * mib, true},
		{"nothing asked", nil, 4608 * mib, false},
		{"nothing reported", gi("5Gi"), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, msg := heapMismatch(c.want, c.reported)
			if got != c.mismatch {
				t.Fatalf("mismatch = %v (%q), want %v", got, msg, c.mismatch)
			}
		})
	}

	_, msg := heapMismatch(gi("5Gi"), 4608*mib)
	for _, s := range []string{"5120", "4608"} {
		if !strings.Contains(msg, s) {
			t.Errorf("message %q does not name %s", msg, s)
		}
	}
}

func TestAServerNamesAHeapOffItsAskOnceAndClearsItWhenItAgrees(t *testing.T) {
	f := newFixture(t)
	f.createServerWith("lobby-heap", func(srv *spawneryv1alpha1.Server) {
		q := resource.MustParse("5Gi")
		srv.Spec.Heap = &q
	})
	uid := bringUpNamed(t, f, "lobby-heap")
	cond := func() *metav1.Condition {
		return meta.FindStatusCondition(f.server("lobby-heap").Status.Conditions, spawneryv1alpha1.ConditionHeapMismatch)
	}
	warnings := func() int {
		n := 0
		for _, e := range f.reconc.Recorder.(*nonBlockingRecorder).events {
			if strings.HasPrefix(e, "Warning HeapMismatch ") {
				n++
			}
		}
		return n
	}

	f.reconcile("lobby-heap")
	if c := cond(); c != nil {
		t.Fatalf("condition = %+v before the agent reported a heap, want none", c)
	}

	const mib = 1 << 20
	if err := f.agents.ReportHeap(uid, 1024*mib, 4608*mib); err != nil {
		t.Fatalf("ReportHeap: %v", err)
	}
	f.reconcile("lobby-heap")
	f.reconcile("lobby-heap")
	c := cond()
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != spawneryv1alpha1.ReasonHeapMismatched ||
		!strings.Contains(c.Message, "5120") || !strings.Contains(c.Message, "4608") {
		t.Fatalf("condition = %+v, want True/HeapMismatched naming 5120 and 4608", c)
	}
	if n := warnings(); n != 1 {
		t.Errorf("%d HeapMismatch warnings over two passes, want 1", n)
	}

	if err := f.agents.ReportHeap(uid, 1024*mib, 5120*mib); err != nil {
		t.Fatalf("ReportHeap: %v", err)
	}
	f.reconcile("lobby-heap")
	if c := cond(); c == nil || c.Status != metav1.ConditionFalse || c.Reason != spawneryv1alpha1.ReasonHeapAsAsked {
		t.Errorf("condition = %+v, want False/HeapAsAsked", c)
	}
}
