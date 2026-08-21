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

// namespacesMissingCA is unexported -- Task 4 calls it from within this
// package -- so this file is white-box (package certs, like bundle_test.go),
// not certs_test like the rest of the envtest suite: an external test
// package cannot reference an unexported method at all, regardless of what
// it does at runtime.
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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

// createNetwork makes ns a namespace the gate looks at, and deletes the
// Network again in a t.Cleanup.
//
// testenv runs one apiserver+etcd per test binary with no
// kube-controller-manager, so nothing ever garbage-collects a Namespace (it
// would sit in Terminating forever) or the objects in it, and
// namespacesMissingCA lists Networks cluster-wide: a Network left behind here
// would block every later test's gate on a namespace that will never receive
// that test's freshly minted CA. Deleting the object (as opposed to the
// namespace) works fine against a bare apiserver, and Network carries no
// finalizer (only ServerFinalizer exists), so this completes immediately.
//
// Registered after testenv.Client's own t.Cleanup(cancel): Go runs cleanups
// LIFO, so this one fires first, while ctx is still live -- confirmed with a
// standalone experiment reproducing that registration order before relying
// on it. This is the package certs counterpart of createNetwork in
// rotation_sequence_envtest_test.go (package certs_test); the two exist
// because an external test package cannot share an unexported helper with
// this one, not because the logic differs.
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

// The gate is driven from the Network objects, not from the ConfigMaps.
//
// "A Network owns its namespace" is the one-per-namespace rule
// (pickNamespaceOwner), not a Kubernetes OwnerReference -- the operator never
// creates a namespace and never owns one -- and the CA ConfigMap deliberately
// carries no owner reference so that it outlives the operator. So a
// spawnery-ca ConfigMap whose Network was deleted stays in its namespace
// forever with whatever bundle it last received. A gate phrased as "every
// managed CA ConfigMap" would wait on that dead namespace until somebody
// cleaned it up by hand, which is to say: a rotation would never complete on
// any cluster where a Network had ever been deleted.
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

	// Has the target CA already: not missing. Written re-encoded -- same DER,
	// different bytes (an added header) -- so this namespace only comes out
	// "not missing" if the comparison actually decodes PEM and hashes the
	// DER. A plain bytes.Contains(data, target) substring check would not
	// find the literal target bytes in here and would misreport this
	// namespace as missing -- confirmed by temporarily mutating
	// namespaceHasCA to do exactly that (see task-3-report.md).
	hasTarget := testenv.Namespace(t, ctx, c)
	network(hasTarget)
	configMap(hasTarget, reencodePEM(t, target), unrelated)

	// Has a Network but not the target CA: missing.
	lacksTarget := testenv.Namespace(t, ctx, c)
	network(lacksTarget)
	configMap(lacksTarget, unrelated)

	// Has a stale ConfigMap, no Network and no pods -- the fully abandoned
	// namespace. Must NOT appear in the result even though its ConfigMap lacks
	// the target: nothing runs there, so the switch strands nobody, and the
	// ConfigMap that outlives everything must not block the rotation forever.
	// (A namespace with no Network but pods still running is the other half of
	// the gate's union and does block; that is
	// TestTheGateAlsoCoversANamespaceWithRunningPodsAndNoNetwork below.)
	orphaned := testenv.Namespace(t, ctx, c)
	configMap(orphaned, stale)

	// Has a Network but the bootstrapper has not written spawnery-ca there
	// yet: absent counts as missing, not as an error. If this branch were
	// mishandled as an error, namespacesMissingCA would return early with a
	// nil slice and no error for the whole call -- indistinguishable from
	// "everything is caught up" -- which is the one outcome a read failure
	// must never produce.
	noConfigMapYet := testenv.Namespace(t, ctx, c)
	network(noConfigMapYet)

	s := &Store{Client: c}
	got, err := s.namespacesMissingCA(ctx, target)
	if err != nil {
		t.Fatalf("namespacesMissingCA: %v", err)
	}

	// Belt to the t.Cleanup's braces: the cleanup above is what keeps this
	// namespace out of some *other* test's result (and out of the annotation
	// production code writes), but this call itself can still observe a
	// still-running or previously-failed-to-clean-up test's namespaces
	// because the control plane and its Networks are shared package-wide.
	// Filtering to this test's own namespaces keeps the assertion below
	// meaningful regardless of what else exists in the cluster; it is not a
	// substitute for the cleanup, which is what stops the leak in the first
	// place.
	own := map[string]bool{hasTarget: true, lacksTarget: true, orphaned: true, noConfigMapYet: true}
	got = slices.DeleteFunc(slices.Clone(got), func(ns string) bool { return !own[ns] })

	want := []string{lacksTarget, noConfigMapYet}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("namespacesMissingCA = %v, want %v (the orphaned namespace %q must be excluded)",
			got, want, orphaned)
	}
}

// reencodePEM decodes a certificate and re-encodes it with an added header,
// so the returned bytes differ from the input while the DER -- and so the
// SHA-256 this package actually compares -- stays identical. Exists to make
// TestTheGateIsDrivenFromNetworksNotConfigMaps catch a comparison that
// matches on PEM bytes instead of on the certificate.
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

// A namespace whose ConfigMap cannot be read at all -- as opposed to one that
// is simply absent -- must come back as an error, not silently as "caught
// up". A nil slice with a nil error is indistinguishable from a rotation
// that is actually clear to proceed, and this is the one outcome
// namespacesMissingCA must never produce.
//
// This needs a client that can be told to fail a Get with something other
// than NotFound, which envtest's real apiserver has no simple way to do
// (that would need a caller with restricted RBAC). A fake client wrapped in
// an interceptor does it directly: intercept Get only for ConfigMap, return
// Forbidden, and leave the Network List path alone.
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

// The blocked-on annotation is quoted verbatim into an event, so it is
// bounded, and the design fixes its wording: at most ten namespace names
// followed by "and N more". Needs no control plane -- it is here rather than
// in the certs_test files because blockedOnNote is unexported and this is the
// package's white-box rotation file.
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

// A blocked gate records a Warning naming the namespace it is waiting on --
// asserted against a recorder the test controls, not against the annotation
// alone, because the annotation and the event are two independent writes
// (drivePhase's since == "" branch) and a change that broke one while
// leaving the other would pass every other test in this package.
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

// drop-old ends a rotation, and the design's fifth event says so on the
// secret: RotationCompleted. It also has to leave both gauges reading a
// finished rotation, not a running one -- the brief named this regression by
// name ("a gauge left at its last value after drop-old reports a finished
// rotation as one still running"), so this test seeds both gauges with the
// values a switched rotation actually leaves behind before calling drop-old,
// rather than relying on whatever a prior test in this binary happened to
// leave in the shared, package-level metric state. Seeding is what makes this
// catch a dropped Set/setRotationPhase call: both gauges already read the
// post-drop-old values by coincidence if nothing seeds them first.
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

	// What a rotation that just switched actually leaves the gauges reading:
	// phase=switched, and some namespace count from whenever the gate last
	// ran (3, an arbitrary non-zero stand-in -- the exact figure does not
	// matter, only that it is not the 0 drop-old must leave behind).
	setRotationPhase(PhaseSwitched)
	RotationBlockedNamespaces.Set(3)

	// applyRequest is unexported -- called directly here the same way
	// AdvanceRotation calls it, rather than through the annotation, since
	// this file already reaches into the package for drivePhase above and a
	// second route to the same call would test nothing new.
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
// the phase asked for, and force-deletes it again in a t.Cleanup.
//
// The force is not a detail. envtest runs no kubelet, so an ordinary Delete
// only stamps a DeletionTimestamp and the pod sits in Terminating for the
// rest of the binary's life -- and a terminating pod still counts to the
// gate, which is the whole point of the predicate under test. Deleting with
// a zero grace period is what actually removes it from etcd, and without it
// this helper would leak exactly the object that blocks every later test's
// gate on a namespace that will never receive that test's freshly minted CA.
// Same LIFO cleanup ordering argument as createNetwork above: registered
// after testenv.Client's t.Cleanup(cancel), so it runs while ctx is live.
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
	// The apiserver defaults a freshly created pod to Pending, so a test that
	// wants Running or a terminal phase has to say so on the status
	// subresource itself -- there is no kubelet here to do it.
	pod.Status.Phase = phase
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatalf("set pod %s/%s to %s: %v", ns, name, phase, err)
	}
}

// The gate also covers a namespace whose Network was deleted but whose agent
// pods are still running.
//
// ServerGroup and ProxyGroup carry no OwnerReference to the Network, so
// deleting a Network leaves the groups and their pods running, and in such a
// namespace nothing refreshes spawnery-ca any more. Driven from the Networks
// alone the gate skips that namespace, the window elapses, the switch runs,
// and every agent there fails its next handshake. A namespace with running
// agent pods and no Network is exactly a namespace where the switch would
// strand somebody, so it must block the rotation and be named.
//
// Written against the same shared control plane as the test above, so every
// namespace this one cares about is filtered out of the result for the same
// reason -- and every pod it creates is force-deleted in a t.Cleanup, since a
// leaked one would block the gate of every later test in this binary.
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

	// The hole: a running agent pod, no Network, and a spawnery-ca frozen at
	// whatever it last received. Nothing in the operator will ever refresh
	// it, so the switch would strand this pod's agent.
	stranded := testenv.Namespace(t, ctx, c)
	createManagedPod(t, ctx, c, stranded, "lobby-x7k2", corev1.PodRunning)
	configMap(stranded, stale)

	// A running agent pod whose namespace did keep up: not missing. Proves
	// the union adds namespaces to the set the gate reads, and does not turn
	// every pod-bearing namespace into a blocker.
	caughtUp := testenv.Namespace(t, ctx, c)
	createManagedPod(t, ctx, c, caughtUp, "lobby-b3n8", corev1.PodRunning)
	configMap(caughtUp, target)

	// A pod that has finished for good runs no agent and will never run one
	// again, so its namespace is not somewhere the switch can strand
	// anybody. Without this case the predicate could be "any managed pod at
	// all" and the test would not tell.
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
