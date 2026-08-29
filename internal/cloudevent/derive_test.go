/*
Copyright The Spawnery Authors.

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

package cloudevent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func aServer() *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-a3f9", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
	}
}

func TestAServerPhaseTransitionBecomesAnEvent(t *testing.T) {
	ns, ev, ok := Derive(aServer(), corev1.EventTypeNormal, "ReadyGatePassed",
		"phase Starting -> Ready: the agent reported ready")
	if !ok {
		t.Fatal("a phase transition produced no event")
	}
	if ns != "minecraft" {
		t.Errorf("namespace = %q, want the object's own", ns)
	}
	if ev.GetSubject() != "lobby-a3f9" || ev.GetGroup() != "lobby" {
		t.Errorf("subject/group = %q/%q, want lobby-a3f9/lobby", ev.GetSubject(), ev.GetGroup())
	}
	if ev.GetKind() != "ReadyGatePassed" {
		t.Errorf("kind = %q, want the operator's own reason", ev.GetKind())
	}
	// The operator's words, unchanged. Rewording them here is how a chat feed
	// comes to disagree with kubectl about the same event.
	if ev.GetMessage() != "phase Starting -> Ready: the agent reported ready" {
		t.Errorf("message = %q, want the recorded note verbatim", ev.GetMessage())
	}
	if ev.GetWarning() {
		t.Error("a Normal event was marked a warning")
	}
}

func TestAWarningKeepsItsSeverity(t *testing.T) {
	_, ev, ok := Derive(aServer(), corev1.EventTypeWarning, "ReadinessLost", "the pod stopped answering")
	if !ok {
		t.Fatal("a warning produced no event")
	}
	if !ev.GetWarning() {
		t.Error("a Warning event was not marked as one, so the feed cannot tell it apart")
	}
}

func TestAnObjectTheFeedCannotAddressProducesNothing(t *testing.T) {
	// The certs recorder reports on Secrets in spawnery-system, which is not a
	// game namespace and has no agents in it. More to the point, an object
	// this cannot name is one the feed cannot address: a CloudEvent with no
	// namespace has nowhere to go, and inventing one is worse than dropping it.
	if _, _, ok := Derive(nil, corev1.EventTypeNormal, "Whatever", "note"); ok {
		t.Error("a nil object produced an event")
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ca", Namespace: "spawnery-system"}}
	if _, _, ok := Derive(secret, corev1.EventTypeNormal, "Rotated", "the CA rotated"); ok {
		t.Error("a Secret produced an event")
	}
}

func TestAGroupEventNamesTheGroupAsBothSubjectAndGroup(t *testing.T) {
	// So that collapsing has something to group by without a special case:
	// every event has a group, and for a group's own event it is itself.
	g := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft"},
	}

	_, ev, ok := Derive(g, corev1.EventTypeNormal, "ScalingLimited", "at maxReplicas")
	if !ok {
		t.Fatal("a group event produced nothing")
	}
	if ev.GetSubject() != "lobby" || ev.GetGroup() != "lobby" {
		t.Errorf("subject/group = %q/%q, want lobby/lobby", ev.GetSubject(), ev.GetGroup())
	}
}
