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
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/spawnery/spawnery/internal/agentpb"
)

type recordedCall struct {
	regarding         runtime.Object
	eventtype, reason string
	action, note      string
	args              []interface{}
}

type fakeRecorder struct{ calls []recordedCall }

func (f *fakeRecorder) Eventf(
	regarding runtime.Object, related runtime.Object,
	eventtype, reason, action, note string, args ...interface{},
) {
	f.calls = append(f.calls, recordedCall{regarding, eventtype, reason, action, note, args})
}

type fakeSink struct {
	namespaces []string
	events     []*agentpb.CloudEvent
}

func (f *fakeSink) Publish(namespace string, ev *agentpb.CloudEvent) {
	f.namespaces = append(f.namespaces, namespace)
	f.events = append(f.events, ev)
}

func TestTheWrapperStillRecordsToKubernetes(t *testing.T) {
	// The first thing to protect. A wrapper that fed the chat and swallowed
	// the Kubernetes event would be invisible in every test that looks at the
	// feed, and would take `kubectl get events` with it.
	inner, sink := &fakeRecorder{}, &fakeSink{}
	r := Recorder{Inner: inner, Sink: sink}

	r.Eventf(aServer(), nil, corev1.EventTypeNormal, "ReadyGatePassed", "SyncStatus",
		"phase %s -> %s", "Starting", "Ready")

	if len(inner.calls) != 1 {
		t.Fatalf("the inner recorder saw %d calls, want one", len(inner.calls))
	}
	if inner.calls[0].reason != "ReadyGatePassed" {
		t.Errorf("reason = %q, want it passed through unchanged", inner.calls[0].reason)
	}
	// The note reaches Kubernetes unformatted, with its args, exactly as it
	// did before the wrapper existed: the recorder does its own formatting,
	// and pre-formatting here would change what the API server stores.
	if inner.calls[0].note != "phase %s -> %s" || len(inner.calls[0].args) != 2 {
		t.Errorf("note/args = %q/%v, want them passed through untouched",
			inner.calls[0].note, inner.calls[0].args)
	}
}

func TestTheFeedGetsTheSameSentenceKubectlDoes(t *testing.T) {
	// The property section 4.4 asks for, asserted rather than assumed.
	inner, sink := &fakeRecorder{}, &fakeSink{}
	r := Recorder{Inner: inner, Sink: sink}

	r.Eventf(aServer(), nil, corev1.EventTypeNormal, "ReadyGatePassed", "SyncStatus",
		"phase %s -> %s", "Starting", "Ready")

	if len(sink.events) != 1 {
		t.Fatalf("the sink saw %d events, want one", len(sink.events))
	}
	if got := sink.events[0].GetMessage(); got != "phase Starting -> Ready" {
		t.Errorf("the feed's message = %q, want the formatted note", got)
	}
	if sink.namespaces[0] != "minecraft" {
		t.Errorf("published to %q, want the object's namespace", sink.namespaces[0])
	}
}

func TestAnEventTheFeedDoesNotWantIsStillRecorded(t *testing.T) {
	// Derive returns ok=false for a Secret. That must not cost Kubernetes its
	// event: the feed is the optional half.
	inner, sink := &fakeRecorder{}, &fakeSink{}
	r := Recorder{Inner: inner, Sink: sink}

	r.Eventf(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ca", Namespace: "spawnery-system"}},
		nil, corev1.EventTypeNormal, "Rotated", "Rotate", "the CA rotated")

	if len(inner.calls) != 1 {
		t.Errorf("a Secret event was not recorded to Kubernetes: %d calls", len(inner.calls))
	}
	if len(sink.events) != 0 {
		t.Errorf("a Secret event reached the feed: %+v", sink.events)
	}
}

func TestANilSinkIsNotACrash(t *testing.T) {
	// A recorder may be built before the fan-outs exist. A wrapper that
	// panicked on a nil sink would turn an ordering detail into a startup
	// crash, and the ordering is not this type's business.
	inner := &fakeRecorder{}
	r := Recorder{Inner: inner}

	r.Eventf(aServer(), nil, corev1.EventTypeNormal, "ReadyGatePassed", "SyncStatus", "up")

	if len(inner.calls) != 1 {
		t.Error("a nil sink cost Kubernetes its event")
	}
}
