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

package certs

import (
	"context"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

// createNetwork makes ns a namespace the gate looks at. namespacesMissingCA
// lists Networks cluster-wide, so the Network is deleted again in t.Cleanup.
func createNetwork(t *testing.T, ctx context.Context, c client.Client, ns, name string) {
	t.Helper()
	n := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	if err := c.Create(ctx, n); err != nil {
		t.Fatalf("create Network in %s: %v", ns, err)
	}
	t.Cleanup(func() {
		if err := c.Delete(ctx, n); err != nil {
			t.Errorf("cleanup: delete Network in %s: %v", ns, err)
		}
	})
}

// The gate is driven from the Network objects, not from the ConfigMaps: the
// CA ConfigMap has no owner reference and outlives a deleted Network, so a
// ConfigMap-driven gate would wait on that namespace forever.
func TestTheGateIsDrivenFromNetworksNotConfigMaps(t *testing.T) {
	c, ctx := testenv.Client(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	target, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA (target): %v", err)
	}
	unrelated, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA (unrelated): %v", err)
	}
	stale, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA (stale): %v", err)
	}

	network := func(ns string) { createNetwork(t, ctx, c, ns, "net") }
	configMap := func(ns string, caPEM ...[]byte) {
		t.Helper()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podspec.CAConfigMapName,
				Namespace: ns,
				Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
			},
			Data: map[string]string{
				podspec.CAConfigMapKey: string(slices.Concat(caPEM...)),
			},
		}
		if err := c.Create(ctx, cm); err != nil {
			t.Fatalf("create ConfigMap in %s: %v", ns, err)
		}
	}

	// Has the target CA already: not missing. Re-encoded (same DER, other bytes)
	// so a byte comparison would get it wrong.
	hasTarget := testenv.Namespace(t, ctx, c)
	network(hasTarget)
	configMap(hasTarget, reencodePEM(t, target), unrelated)

	// Has a Network but not the target CA: missing.
	lacksTarget := testenv.Namespace(t, ctx, c)
	network(lacksTarget)
	configMap(lacksTarget, unrelated)

	// A stale ConfigMap, no Network and no pods: nothing to strand, not missing.
	orphaned := testenv.Namespace(t, ctx, c)
	configMap(orphaned, stale)

	// Has a Network but no spawnery-ca yet: absent counts as missing, not as an
	// error.
	noConfigMapYet := testenv.Namespace(t, ctx, c)
	network(noConfigMapYet)

	s := &Store{Client: c}
	got, err := s.namespacesMissingCA(ctx, target)
	if err != nil {
		t.Fatalf("namespacesMissingCA: %v", err)
	}

	// Other tests' Networks share the control plane, so filter to this test's.
	own := map[string]bool{hasTarget: true, lacksTarget: true, orphaned: true, noConfigMapYet: true}
	got = slices.DeleteFunc(slices.Clone(got), func(ns string) bool { return !own[ns] })

	want := []string{lacksTarget, noConfigMapYet}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("namespacesMissingCA = %v, want %v (the orphaned namespace %q must be excluded)",
			got, want, orphaned)
	}
}

// reencodePEM changes the PEM bytes but not the DER this package hashes.
func reencodePEM(t *testing.T, certPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("reencodePEM: not PEM")
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:    block.Type,
		Headers: map[string]string{"X-Reencoded-By": "rotation_envtest_test.go"},
		Bytes:   block.Bytes,
	})
}

// A ConfigMap that cannot be read is an error, never "caught up". A fake
// client, because envtest has no simple way to fail a Get with Forbidden.
func TestTheGatePropagatesAnUnreadableConfigMapAsAnError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "broken"},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(net).Build()
	broken := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				return apierrors.NewForbidden(corev1.Resource("configmaps"), key.Name, fmt.Errorf("no rbac"))
			}
			return inner.Get(ctx, key, obj, opts...)
		},
	})

	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	target, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}

	s := &Store{Client: broken}
	got, err := s.namespacesMissingCA(context.Background(), target)
	if err == nil {
		t.Fatalf("namespacesMissingCA returned (%v, nil); a Get failure that is not NotFound must surface as an error", got)
	}
	if got != nil {
		t.Errorf("namespacesMissingCA returned a non-nil result (%v) alongside an error", got)
	}
}

// The blocked-on annotation is quoted into an event, so it is bounded: at most
// ten namespace names followed by "and N more".
func TestBlockedOnNoteNamesAtMostTenNamespaces(t *testing.T) {
	if got, want := blockedOnNote([]string{"alpha", "beta"}), "alpha,beta"; got != want {
		t.Errorf("blockedOnNote(two) = %q, want %q with no summary tacked on", got, want)
	}

	many := make([]string, 0, 13)
	for i := range 13 {
		many = append(many, fmt.Sprintf("namespace-%02d", i))
	}
	got := blockedOnNote(many)
	if !strings.HasSuffix(got, "and 3 more") {
		t.Errorf("blockedOnNote(13) = %q, want it to end in \"and 3 more\"", got)
	}
	if n := strings.Count(got, "namespace-"); n != maxBlockedNamesInAnnotation {
		t.Errorf("blockedOnNote(13) names %d namespaces, want %d: an annotation is not an "+
			"unbounded field, and Task 5 puts this string in an event note", n, maxBlockedNamesInAnnotation)
	}
}

// A blocked gate records a Warning naming the namespace it waits on. The
// annotation and the event are independent writes, so both are asserted.
func TestBlockedGateRecordsAWarningNamingTheNamespaces(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	rec := events.NewFakeRecorder(10)
	s := &Store{
		Client:               c,
		Namespace:            ns,
		Name:                 SecretName,
		DNSNames:             ServingDNSNames("spawnery-operator", ns),
		Clock:                func() time.Time { return now },
		AgentSessionDeadline: 10 * time.Minute,
		Recorder:             rec,
	}

	current, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// A Network in ns, and deliberately no spawnery-ca ConfigMap to go with
	// it: this is what "missing" means to namespacesMissingCA.
	createNetwork(t, ctx, c, ns, "net")
	nextCert, nextKey, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}
	distributing := current.WithNextCA(nextCert, nextKey)

	if _, _, err := s.drivePhase(ctx, distributing, PhaseDistributing, "", ""); err != nil {
		t.Fatalf("drivePhase: %v", err)
	}

	select {
	case got := <-rec.Events:
		want := corev1.EventTypeWarning + " " + ReasonRotationBlocked + " "
		if !strings.HasPrefix(got, want) {
			t.Fatalf("event = %q, want it to start %q", got, want)
		}
		if !strings.Contains(got, ns) {
			t.Errorf("event = %q, want it to name the blocked namespace %q", got, ns)
		}
	default:
		t.Fatal("no event was recorded for the blocked gate")
	}
}

// drop-old records RotationCompleted and resets both gauges. They are seeded
// first, since the gauges are package-level and could already read the
// post-drop-old values.
func TestDropOldRecordsRotationCompleted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	rec := events.NewFakeRecorder(10)
	s := &Store{
		Client:               c,
		Namespace:            ns,
		Name:                 SecretName,
		DNSNames:             ServingDNSNames("spawnery-operator", ns),
		Clock:                func() time.Time { return now },
		AgentSessionDeadline: 10 * time.Minute,
		Recorder:             rec,
	}

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	nextCert, nextKey, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}
	switched, err := first.WithNextCA(nextCert, nextKey).SwitchToNext(now, s.DNSNames)
	if err != nil {
		t.Fatalf("SwitchToNext: %v", err)
	}

	// What a switched rotation leaves behind; 3 is any non-zero count.
	setRotationPhase(PhaseSwitched)
	RotationBlockedNamespaces.Set(3)

	if _, _, err := s.applyRequest(ctx, switched, RequestDropOld, PhaseSwitched); err != nil {
		t.Fatalf("applyRequest (drop-old): %v", err)
	}

	select {
	case got := <-rec.Events:
		want := corev1.EventTypeNormal + " " + ReasonRotationCompleted + " "
		if !strings.HasPrefix(got, want) {
			t.Fatalf("event = %q, want it to start %q", got, want)
		}
	default:
		t.Fatal("no event was recorded for the completed rotation")
	}

	if got := testutil.ToFloat64(RotationPhase.WithLabelValues(phaseNone)); got != 1 {
		t.Errorf("RotationPhase{phase=%q} = %v after drop-old, want 1", phaseNone, got)
	}
	if got := testutil.ToFloat64(RotationPhase.WithLabelValues(PhaseSwitched)); got != 0 {
		t.Errorf("RotationPhase{phase=%q} = %v after drop-old, want 0 -- "+
			"the rotation that just ended", PhaseSwitched, got)
	}
	if got := testutil.ToFloat64(RotationBlockedNamespaces); got != 0 {
		t.Errorf("RotationBlockedNamespaces = %v after drop-old, want 0 -- "+
			"a gauge left at its last value reports a finished rotation as one still running", got)
	}
}

// createManagedPod puts a pod carrying podspec.LabelManagedBy in ns, gives it
// the phase asked for, and force-deletes it again in a t.Cleanup. Without a
// kubelet a plain Delete leaves it Terminating, and that still counts to the gate.
func createManagedPod(t *testing.T, ctx context.Context, c client.Client, ns, name string, phase corev1.PodPhase) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelRole:      podspec.RoleServer,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "minecraft", Image: "example.invalid/paper:latest"}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("create pod %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() {
		if err := c.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
			t.Errorf("cleanup: delete pod %s/%s: %v", ns, name, err)
		}
	})
	// No kubelet: a phase other than Pending is set on the status subresource.
	pod.Status.Phase = phase
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatalf("set pod %s/%s to %s: %v", ns, name, phase, err)
	}
}

// The gate also covers a namespace whose Network was deleted but whose agent
// pods are still running: the groups have no OwnerReference to the Network,
// so they outlive it, and nothing refreshes spawnery-ca there.
func TestTheGateAlsoCoversANamespaceWithRunningPodsAndNoNetwork(t *testing.T) {
	c, ctx := testenv.Client(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	target, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA (target): %v", err)
	}
	stale, _, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA (stale): %v", err)
	}
	configMap := func(ns string, caPEM []byte) {
		t.Helper()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podspec.CAConfigMapName,
				Namespace: ns,
				Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
			},
			Data: map[string]string{podspec.CAConfigMapKey: string(caPEM)},
		}
		if err := c.Create(ctx, cm); err != nil {
			t.Fatalf("create ConfigMap in %s: %v", ns, err)
		}
	}

	// A running agent pod, no Network: nothing will refresh its spawnery-ca.
	stranded := testenv.Namespace(t, ctx, c)
	createManagedPod(t, ctx, c, stranded, "lobby-x7k2", corev1.PodRunning)
	configMap(stranded, stale)

	// A running agent pod in a namespace that kept up: not missing.
	caughtUp := testenv.Namespace(t, ctx, c)
	createManagedPod(t, ctx, c, caughtUp, "lobby-b3n8", corev1.PodRunning)
	configMap(caughtUp, target)

	// A finished pod runs no agent: not missing.
	finished := testenv.Namespace(t, ctx, c)
	createManagedPod(t, ctx, c, finished, "lobby-done", corev1.PodSucceeded)
	configMap(finished, stale)

	s := &Store{Client: c}
	got, err := s.namespacesMissingCA(ctx, target)
	if err != nil {
		t.Fatalf("namespacesMissingCA: %v", err)
	}

	own := map[string]bool{stranded: true, caughtUp: true, finished: true}
	got = slices.DeleteFunc(slices.Clone(got), func(ns string) bool { return !own[ns] })

	want := []string{stranded}
	if !slices.Equal(got, want) {
		t.Errorf("namespacesMissingCA = %v, want %v: %q holds a running agent pod and a "+
			"stale spawnery-ca that nothing will ever refresh, so switching would strand it; "+
			"%q has caught up and %q runs no process",
			got, want, stranded, caughtUp, finished)
	}
}

// newRecordingStore is the fixture the event tests share: a Store on its own
// namespace with a frozen clock and a recorder the test drains. Buffered, so an
// unexpected second event shows up instead of blocking production code.
func newRecordingStore(t *testing.T) (*Store, *events.FakeRecorder, context.Context, string, time.Time) {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	rec := events.NewFakeRecorder(10)
	return &Store{
		Client:               c,
		Namespace:            ns,
		Name:                 SecretName,
		DNSNames:             ServingDNSNames("spawnery-operator", ns),
		Clock:                func() time.Time { return now },
		AgentSessionDeadline: 10 * time.Minute,
		Recorder:             rec,
	}, rec, ctx, ns, now
}

// expectEvent takes the one recorded event, checks its type and reason, and
// returns its text; it fails rather than hangs when nothing was recorded.
func expectEvent(t *testing.T, rec *events.FakeRecorder, eventtype, reason string) string {
	t.Helper()
	select {
	case got := <-rec.Events:
		if want := eventtype + " " + reason + " "; !strings.HasPrefix(got, want) {
			t.Fatalf("event = %q, want it to start %q", got, want)
		}
		return got
	default:
		t.Fatalf("no %s %s event was recorded", eventtype, reason)
		return ""
	}
}

// start says so on the secret.
func TestStartRecordsRotationStarted(t *testing.T) {
	s, rec, ctx, ns, _ := newRecordingStore(t)

	current, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	fresh, inFlight, err := s.applyRequest(ctx, current, RequestStart, "")
	if err != nil {
		t.Fatalf("applyRequest (start): %v", err)
	}
	if !inFlight || len(fresh.NextCACertPEM) == 0 {
		t.Fatalf("start did not mint an incoming CA (inFlight=%v)", inFlight)
	}
	if got := secretAnnotation(t, ctx, s, ns, AnnotationRotationPhase); got != PhaseDistributing {
		t.Fatalf("phase = %q, want %q", got, PhaseDistributing)
	}

	got := expectEvent(t, rec, corev1.EventTypeNormal, ReasonRotationStarted)
	if !strings.Contains(got, keyNextCACert) {
		t.Errorf("event = %q, want it to name %s, the slot a human would go and look at",
			got, keyNextCACert)
	}
}

// The switch says so on the secret too.
func TestTheSwitchRecordsRotationSwitched(t *testing.T) {
	s, rec, ctx, _, now := newRecordingStore(t)

	current, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	nextCert, nextKey, err := IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}
	distributing := current.WithNextCA(nextCert, nextKey)

	// `since` stamped an hour ago, so this call is the switch itself; the gate
	// does not run again once stamped.
	since := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	fresh, inFlight, err := s.drivePhase(ctx, distributing, PhaseDistributing, since, "")
	if err != nil {
		t.Fatalf("drivePhase: %v", err)
	}
	if !inFlight || len(fresh.NextCACertPEM) != 0 {
		t.Fatalf("the switch did not happen (inFlight=%v, next slot occupied=%v)",
			inFlight, len(fresh.NextCACertPEM) != 0)
	}

	got := expectEvent(t, rec, corev1.EventTypeNormal, ReasonRotationSwitched)
	if !strings.Contains(got, RequestDropOld) {
		t.Errorf("event = %q, want it to name %s, the request that ends the rotation from here",
			got, RequestDropOld)
	}
}

// A rotate-ca value the operator cannot read is reported on the secret, not
// only in the log: the sequence carries on, so the event is the only signal.
func TestAnUnrecognisedRequestRecordsAWarning(t *testing.T) {
	s, rec, ctx, ns, _ := newRecordingStore(t)

	current, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	annotate(t, ctx, s, ns, AnnotationRotateRequest, "strat")

	if _, _, err := s.AdvanceRotation(ctx, current); err != nil {
		t.Fatalf("AdvanceRotation with an unreadable request: %v", err)
	}
	if got := secretAnnotation(t, ctx, s, ns, AnnotationRotateRequest); got != "strat" {
		t.Fatalf("rotate-ca = %q, want it left in place", got)
	}

	got := expectEvent(t, rec, corev1.EventTypeWarning, ReasonRotationRequestUnrecognised)
	if !strings.Contains(got, "strat") {
		t.Errorf("event = %q, want it to quote the value that was typed", got)
	}
}

// A request the operator understands and will not perform is reported too:
// the refusal only deletes the annotation, so the event is its only trace.
func TestARefusedRequestRecordsAWarning(t *testing.T) {
	s, rec, ctx, ns, _ := newRecordingStore(t)

	current, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	annotate(t, ctx, s, ns, AnnotationRotateRequest, RequestDropOld)

	// distributing, not switched: the CA drop-old would remove is the one
	// signing the serving certificate right now.
	if _, _, err := s.applyRequest(ctx, current, RequestDropOld, PhaseDistributing); err == nil {
		t.Fatal("applyRequest (drop-old during distributing) did not refuse")
	}
	if got := secretAnnotation(t, ctx, s, ns, AnnotationRotateRequest); got != "" {
		t.Fatalf("rotate-ca = %q after a refusal, want it consumed like an accepted request", got)
	}

	got := expectEvent(t, rec, corev1.EventTypeWarning, ReasonRotationRequestRefused)
	if !strings.Contains(got, PhaseDistributing) {
		t.Errorf("event = %q, want it to name the phase that refused the request", got)
	}
	if !strings.Contains(got, "consumed") {
		t.Errorf("event = %q, want it to say the request was consumed -- the human is "+
			"looking at an annotation that has just vanished", got)
	}
}

func annotate(t *testing.T, ctx context.Context, s *Store, ns, key, value string) {
	t.Helper()
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: s.Name, Namespace: ns}, secret); err != nil {
		t.Fatalf("get the secret: %v", err)
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[key] = value
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("annotate the secret with %s=%s: %v", key, value, err)
	}
}

func secretAnnotation(t *testing.T, ctx context.Context, s *Store, ns, key string) string {
	t.Helper()
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: s.Name, Namespace: ns}, secret); err != nil {
		t.Fatalf("get the secret: %v", err)
	}
	return secret.Annotations[key]
}
