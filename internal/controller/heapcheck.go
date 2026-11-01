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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
)

const heapTolerancePercent = 5

// heapMismatch compares the heap a Server asked for with the maximum its agent
// reports in bytes, 0 when unknown.
func heapMismatch(want *resource.Quantity, reported int64) (bool, string) {
	if want == nil || reported <= 0 {
		return false, ""
	}
	diff := reported - want.Value()
	if diff < 0 {
		diff = -diff
	}
	if diff*100 <= want.Value()*heapTolerancePercent {
		return false, ""
	}
	const mib = 1 << 20
	return true, fmt.Sprintf("the agent reports a maximum heap of %dMi, spec.heap asks for %dMi",
		reported/mib, want.Value()/mib)
}

// checkHeap leaves the condition alone until there is both a spec.heap and a
// report to compare.
func (r *ServerReconciler) checkHeap(srv *spawneryv1alpha1.Server, snap agent.Snapshot) {
	if srv.Spec.Heap == nil || !snap.Known || snap.HeapMax <= 0 {
		return
	}
	mismatch, message := heapMismatch(srv.Spec.Heap, snap.HeapMax)
	if !mismatch {
		meta.SetStatusCondition(&srv.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionHeapMismatch,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonHeapAsAsked,
			Message: "the agent's maximum heap is within 5 % of spec.heap",
		})
		return
	}
	if !meta.IsStatusConditionTrue(srv.Status.Conditions, spawneryv1alpha1.ConditionHeapMismatch) {
		r.Recorder.Eventf(srv, nil, corev1.EventTypeWarning, "HeapMismatch", actionSyncStatus, "%s", message)
	}
	meta.SetStatusCondition(&srv.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionHeapMismatch,
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonHeapMismatched,
		Message: message,
	})
}
