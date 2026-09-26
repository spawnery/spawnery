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
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
)

func TestAnUnretiredServerComesBackAndIsNotRetiredAgain(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setUpdatePolicy(t, 1, &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1})
	f.reconcileNamedGroup(t, r, "lobby")
	f.readyAllServersOf(t, "lobby")
	name := f.serverNamesOfGroup(t, "lobby")[0]
	f.reportPlayersOn(t, name, 3)

	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, r, "lobby")
	f.bringUpCurrent(t, "lobby")
	f.reconcileNamedGroup(t, r, "lobby")
	f.reconcile(name)
	// The group sees its own retirement land, so its reservation is settled
	// and only spec.hold can keep the next pass from retiring the server again.
	f.reconcileNamedGroup(t, r, "lobby")
	if got := f.server(name).Status.Phase; got != string(phase.Retiring) {
		t.Fatalf("phase = %s, want Retiring before the unretire", got)
	}

	srv := f.server(name)
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Retire, srv.Spec.Hold = false, true
	if err := f.c.Patch(f.ctx, srv, patch); err != nil {
		t.Fatalf("patch: %v", err)
	}
	f.reconcile(name)
	f.reconcileNamedGroup(t, r, "lobby")
	f.reconcileNamedGroup(t, r, "lobby")

	got := f.server(name)
	if got.Status.Phase != string(phase.Ready) || !got.Status.Registered || got.Status.RetiringSince != nil {
		t.Errorf("phase=%s registered=%v retiringSince=%v, want Ready, registered, cleared",
			got.Status.Phase, got.Status.Registered, got.Status.RetiringSince)
	}
	if got.Spec.Retire {
		t.Error("the roll retired the held server again")
	}
}
