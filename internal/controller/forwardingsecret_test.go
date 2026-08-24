package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// stubReader answers every Get with the same secret or the same error, which
// is what makes Forbidden testable at all: a test running against envtest holds
// admin credentials and can never be denied.
type stubReader struct {
	secret *corev1.Secret
	err    error
}

func (s stubReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if s.err != nil {
		return s.err
	}
	*obj.(*corev1.Secret) = *s.secret
	return nil
}

func (s stubReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	panic("readForwardingSecret must not list")
}

func testNetworkForSecret() *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "mc", UID: "u-1"},
		Spec:       spawneryv1alpha1.NetworkSpec{ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "fwd"}},
	}
}

func secretsResource() schema.GroupResource {
	return schema.GroupResource{Resource: "secrets"}
}

// Each read outcome has its own remedy, so each gets its own reason. A single
// "could not read it" would send a user with a typo and a user with a missing
// RoleBinding to the same place.
func TestReadForwardingSecretNamesEachOutcome(t *testing.T) {
	net := testNetworkForSecret()

	for _, tc := range []struct {
		name       string
		reader     stubReader
		wantStatus metav1.ConditionStatus
		wantReason string
		wantHash   bool
	}{
		{
			name: "readable",
			reader: stubReader{secret: &corev1.Secret{
				Data: map[string][]byte{podspec.ForwardingSecretKey: []byte("s3cret")},
			}},
			wantStatus: metav1.ConditionTrue,
			wantReason: spawneryv1alpha1.ReasonSecretResolved,
			wantHash:   true,
		},
		{
			name:       "absent",
			reader:     stubReader{err: apierrors.NewNotFound(secretsResource(), "fwd")},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonSecretNotFound,
		},
		{
			name:       "denied",
			reader:     stubReader{err: apierrors.NewForbidden(secretsResource(), "fwd", nil)},
			wantStatus: metav1.ConditionUnknown,
			wantReason: spawneryv1alpha1.ReasonSecretReadForbidden,
		},
		{
			name:       "api down",
			reader:     stubReader{err: apierrors.NewServiceUnavailable("etcd is having a day")},
			wantStatus: metav1.ConditionUnknown,
			wantReason: spawneryv1alpha1.ReasonSecretReadFailed,
		},
		{
			name:       "no key",
			reader:     stubReader{secret: &corev1.Secret{Data: map[string][]byte{"other": []byte("x")}}},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonSecretKeyMissing,
		},
		{
			name: "empty key",
			reader: stubReader{secret: &corev1.Secret{
				Data: map[string][]byte{podspec.ForwardingSecretKey: {}},
			}},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonSecretKeyMissing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readForwardingSecret(context.Background(), tc.reader, net)

			if got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Errorf("read = %s/%s, want %s/%s", got.Status, got.Reason, tc.wantStatus, tc.wantReason)
			}
			if (got.Hash != "") != tc.wantHash {
				t.Errorf("hash = %q, want non-empty: %v", got.Hash, tc.wantHash)
			}
			if got.Message == "" {
				t.Error("message is empty; every outcome has to say what happened")
			}
		})
	}
}

// The Forbidden message is the only place an administrator learns that an
// install step was skipped, so it has to name the manifest.
func TestForbiddenNamesTheManifestToApply(t *testing.T) {
	got := readForwardingSecret(context.Background(),
		stubReader{err: apierrors.NewForbidden(secretsResource(), "fwd", nil)}, testNetworkForSecret())

	if !strings.Contains(got.Message, "config/rbac/forwarding-secret-reader.yaml") {
		t.Errorf("message = %q, want it to name config/rbac/forwarding-secret-reader.yaml", got.Message)
	}
	if !strings.Contains(got.Message, "mc") {
		t.Errorf("message = %q, want it to name the namespace so the apply can be copied", got.Message)
	}
}

func stampPod(group, role, hash string, terminating bool) corev1.Pod {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      group + "-" + role,
		Namespace: "mc",
		Labels: map[string]string{
			podspec.LabelGroup: group,
			podspec.LabelRole:  role,
		},
	}}
	if hash != "" {
		pod.Labels[podspec.LabelForwardingHash] = hash
	}
	if terminating {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"test/hold"}
	}
	return pod
}

func TestForwardingStampsSkipTerminatingPods(t *testing.T) {
	got := forwardingStamps([]corev1.Pod{
		stampPod("lobby", podspec.RoleServer, "aaaa", false),
		stampPod("edge", podspec.RoleProxy, "aaaa", true),
	})

	if len(got) != 1 || got[0].Group != "lobby" {
		t.Errorf("stamps = %+v, want only the lobby pod; a pod on its way out must not hold the report open", got)
	}
}

// A failed Server keeps its pod for spec.failedRetentionSeconds — an hour by
// default — and that pod carries no DeletionTimestamp. Counting it would hold
// RotationPending at True for the whole retention window after a rotation that
// is otherwise complete, and name its group as work still to do.
func TestForwardingStampsSkipTerminalPods(t *testing.T) {
	failed := stampPod("survival", podspec.RoleServer, "aaaa", false)
	failed.Status.Phase = corev1.PodFailed

	// The container the crash-loop check reads is podspec.ContainerName; a
	// restart count below MaxContainerRestarts, or any other container name,
	// does not trip it.
	looping := stampPod("minigames", podspec.RoleServer, "aaaa", false)
	looping.Status.Phase = corev1.PodRunning
	looping.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name:         "sidecar",
			RestartCount: MaxContainerRestarts,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		},
		{
			Name:         podspec.ContainerName,
			RestartCount: MaxContainerRestarts,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		},
	}
	if !crashLooping(&looping) {
		t.Fatalf("the crash-loop fixture does not trip crashLooping, so this test would pass for the wrong reason")
	}

	got := forwardingStamps([]corev1.Pod{
		stampPod("lobby", podspec.RoleServer, "bbbb", false),
		failed,
		looping,
	})

	if len(got) != 1 || got[0].Group != "lobby" {
		t.Errorf("stamps = %+v, want only the lobby pod; a pod whose process is down runs no forwarding secret", got)
	}
}

func TestRotationConditionFollowsItsPrecedence(t *testing.T) {
	resolved := forwardingRead{Hash: "aaaa", Status: metav1.ConditionTrue, Reason: spawneryv1alpha1.ReasonSecretResolved}
	unresolved := forwardingRead{Status: metav1.ConditionUnknown, Reason: spawneryv1alpha1.ReasonSecretNotFound, Message: "no such secret"}

	for _, tc := range []struct {
		name       string
		read       forwardingRead
		pods       []corev1.Pod
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "an unreadable secret outranks everything",
			read:       unresolved,
			pods:       []corev1.Pod{stampPod("lobby", podspec.RoleServer, "bbbb", false)},
			wantStatus: metav1.ConditionUnknown,
			wantReason: spawneryv1alpha1.ReasonSecretUnresolved,
		},
		{
			name:       "a stale pod outranks an unstamped one",
			read:       resolved,
			pods:       []corev1.Pod{stampPod("lobby", podspec.RoleServer, "bbbb", false), stampPod("edge", podspec.RoleProxy, "", false)},
			wantStatus: metav1.ConditionTrue,
			wantReason: spawneryv1alpha1.ReasonRotationPending,
		},
		{
			name:       "an unstamped pod alone is unknown, never pending",
			read:       resolved,
			pods:       []corev1.Pod{stampPod("lobby", podspec.RoleServer, "", false)},
			wantStatus: metav1.ConditionUnknown,
			wantReason: spawneryv1alpha1.ReasonPodsPredateTracking,
		},
		{
			name:       "all current is in sync",
			read:       resolved,
			pods:       []corev1.Pod{stampPod("lobby", podspec.RoleServer, "aaaa", false)},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonForwardingSecretInSync,
		},
		{
			name:       "no pods at all is vacuously in sync",
			read:       resolved,
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonForwardingSecretInSync,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rotationCondition(tc.read, forwardingStamps(tc.pods))

			if got.Type != spawneryv1alpha1.ConditionForwardingSecretRotationPending {
				t.Errorf("type = %q, want %q", got.Type, spawneryv1alpha1.ConditionForwardingSecretRotationPending)
			}
			if got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Errorf("condition = %s/%s, want %s/%s", got.Status, got.Reason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// The message is read by somebody about to execute the roll, so it lists the
// work in the order they are to do it: every server group before every proxy
// group, each sorted by name.
func TestRotationMessageListsServersBeforeProxies(t *testing.T) {
	read := forwardingRead{Hash: "aaaa", Status: metav1.ConditionTrue, Reason: spawneryv1alpha1.ReasonSecretResolved}
	got := rotationCondition(read, forwardingStamps([]corev1.Pod{
		stampPod("edge", podspec.RoleProxy, "bbbb", false),
		stampPod("survival", podspec.RoleServer, "bbbb", false),
		stampPod("lobby", podspec.RoleServer, "bbbb", false),
	}))

	want := "server/lobby=1, server/survival=1, proxy/edge=1"
	if !strings.Contains(got.Message, want) {
		t.Errorf("message = %q, want it to contain %q", got.Message, want)
	}
	if !strings.Contains(got.Message, rotationRunbook) {
		t.Errorf("message = %q, want it to name %s", got.Message, rotationRunbook)
	}
}

// A group answering for itself, in the same vocabulary its Network uses.
//
// The Network carries the fleet sum and names which groups are behind, in a
// message; until now that message was the only place the information existed,
// so `kubectl get servergroup` said nothing about a rotation and a per-group
// alert had to parse a string. This is the group's own answer, and it follows
// the same precedence for the same reason — a known problem outranks an
// unknown one.
func TestAGroupAnswersForItsOwnPods(t *testing.T) {
	for _, tc := range []struct {
		name        string
		networkHash string
		pods        []corev1.Pod
		wantStatus  metav1.ConditionStatus
		wantReason  string
	}{
		{
			// Strictly downstream of the Network's own report: the group
			// controllers hold no reader for the secret and no grant on it, so
			// a network that published no digest leaves every group in it
			// saying so rather than guessing.
			name:        "no digest published means the group cannot tell",
			networkHash: "",
			pods:        []corev1.Pod{stampPod("lobby", podspec.RoleServer, "bbbb", false)},
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  spawneryv1alpha1.ReasonSecretUnresolved,
		},
		{
			name:        "a stale pod outranks an unstamped one",
			networkHash: "aaaa",
			pods: []corev1.Pod{
				stampPod("lobby", podspec.RoleServer, "bbbb", false),
				stampPod("lobby", podspec.RoleServer, "", false),
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: spawneryv1alpha1.ReasonRotationPending,
		},
		{
			name:        "an unstamped pod alone is unknown, never pending",
			networkHash: "aaaa",
			pods:        []corev1.Pod{stampPod("lobby", podspec.RoleServer, "", false)},
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  spawneryv1alpha1.ReasonPodsPredateTracking,
		},
		{
			name:        "every pod current",
			networkHash: "aaaa",
			pods:        []corev1.Pod{stampPod("lobby", podspec.RoleServer, "aaaa", false)},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  spawneryv1alpha1.ReasonForwardingSecretInSync,
		},
		{
			// A group with no pods at all is in sync by having nothing that
			// is not: a parked persistent group must not read as pending.
			name:        "no pods at all",
			networkHash: "aaaa",
			wantStatus:  metav1.ConditionFalse,
			wantReason:  spawneryv1alpha1.ReasonForwardingSecretInSync,
		},
		{
			// forwardingStamps drops it, and this is here so that rule is
			// pinned on the group path too: a pod on its way out must not hold
			// the report open after the replacement that fixes it exists.
			name:        "a terminating pod does not hold the report open",
			networkHash: "aaaa",
			pods: []corev1.Pod{
				stampPod("lobby", podspec.RoleServer, "aaaa", false),
				stampPod("lobby", podspec.RoleServer, "bbbb", true),
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonForwardingSecretInSync,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conditions []metav1.Condition
			reportGroupRotation(&conditions, tc.networkHash, tc.pods)
			got := meta.FindStatusCondition(conditions,
				spawneryv1alpha1.ConditionForwardingSecretRotationPending)
			if got == nil {
				t.Fatal("no ForwardingSecretRotationPending condition was set at all")
			}
			if got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Errorf("condition = %s/%s, want %s/%s: %s",
					got.Status, got.Reason, tc.wantStatus, tc.wantReason, got.Message)
			}
		})
	}
}
