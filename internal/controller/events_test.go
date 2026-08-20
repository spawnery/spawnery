package controller

import (
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

// TestEventNoteLeavesAShortNoteAlone is the half that protects the other 18
// call sites. eventNote sits in front of text this operator did not write, so
// the overwhelmingly common case is a note well inside the limit -- and if the
// helper reworded, re-wrapped or re-encoded those, every event assertion in
// this package would be reading something other than what the controller said.
func TestEventNoteLeavesAShortNoteAlone(t *testing.T) {
	for _, note := range []string{
		"",
		"created pod lobby-x7k2",
		"the forwarding secret changed; roll the server groups first — see the runbook",
		strings.Repeat("x", maxEventNote),
	} {
		if got := eventNote("%s", note); got != note {
			t.Errorf("eventNote(%q) = %q, want it returned byte for byte", note, got)
		}
	}
}

// TestEventNoteTruncatesAndSaysSo pins both halves of the contract: the result
// fits, and it admits to being cut. A silently shortened API server message is
// worse than a long one, because a reader cannot tell a sentence the API server
// ended from one this operator cut.
func TestEventNoteTruncatesAndSaysSo(t *testing.T) {
	// A stand-in for a PodSecurity violation list, which is the real case:
	// long, and worth reading precisely because the remedy is in its wording.
	long := "the API server refused a proxy pod: " + strings.Repeat("violates PodSecurity restricted:v1; ", 60)
	got := eventNote("%s", long)

	if len(got) > maxEventNote {
		t.Errorf("len = %d, want <= %d — this is the event the API server drops", len(got), maxEventNote)
	}
	if !strings.HasSuffix(got, eventNoteTruncated) {
		t.Errorf("got = %q, want it to end with the truncation marker %q", got, eventNoteTruncated)
	}
	if !strings.Contains(got, "status conditions") {
		t.Errorf("got = %q, want the marker to point at where the full text is", got)
	}
	// The head must still be the API server's own words, unaltered.
	if !strings.HasPrefix(got, "the API server refused a proxy pod: violates PodSecurity") {
		t.Errorf("got = %q, want the surviving head to be the original text", got)
	}
}

// TestEventNoteCutsOnARuneBoundary guards the byte/character trap. The limit is
// counted in bytes, so a note of multi-byte runes hits it at well under 1024
// characters -- and a cut that lands mid-rune produces invalid UTF-8, trading a
// too-long event for a differently-invalid one.
func TestEventNoteCutsOnARuneBoundary(t *testing.T) {
	// Em-dashes are 3 bytes each, so this is 1500 bytes of 500 characters.
	got := eventNote("%s", strings.Repeat("—", 500))
	if len(got) > maxEventNote {
		t.Errorf("len = %d bytes, want <= %d", len(got), maxEventNote)
	}
	if !utf8.ValidString(got) {
		t.Errorf("got = %q, want valid UTF-8 — the cut split a rune", got)
	}
}

// TestTheRealAPIServerAcceptsATruncatedNoteAndRefusesAnUntruncatedOne is the
// assertion that would actually have caught this regression.
//
// Every other event assertion in this package reads FakeRecorder, which
// validates nothing and would accept a note of any length -- which is exactly
// why the migration onto events.k8s.io/v1 could introduce a dropped-event bug
// that the whole suite stayed green through. This one goes to envtest's real
// API server, so it fails if the limit moves, if eventNote stops enforcing it,
// or if the note ever again reaches Eventf unbounded.
func TestTheRealAPIServerAcceptsATruncatedNoteAndRefusesAnUntruncatedOne(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	create := func(name, note string) error {
		return c.Create(ctx, &eventsv1.Event{
			ObjectMeta:          metav1.ObjectMeta{Name: name, Namespace: ns},
			EventTime:           metav1.NowMicro(),
			ReportingController: "spawnery.cloud/proxygroup",
			ReportingInstance:   "spawnery-operator-0",
			Action:              actionCreateProxyPod,
			Reason:              "ProxyPodBlocked",
			Type:                corev1.EventTypeWarning,
			Regarding: corev1.ObjectReference{
				Kind: "Namespace", Name: ns, Namespace: ns, APIVersion: "v1",
			},
			Note: note,
		})
	}

	// The note the refused-proxy-pod site would build from a long admission
	// refusal, before eventNote sees it.
	raw := "the API server refused a proxy pod: " +
		strings.Repeat("violates PodSecurity restricted:v1; ", 60)
	if len(raw) <= maxEventNote {
		t.Fatalf("the fixture is %d bytes, which is not over the limit it is meant to exceed", len(raw))
	}

	if err := create("untruncated", raw); err == nil {
		t.Error("the API server accepted a note over the limit; " +
			"if the limit has been lifted, eventNote and its comment need revisiting")
	} else if !strings.Contains(err.Error(), "note") && !strings.Contains(err.Error(), "message") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	if err := create("truncated", eventNote("%s", raw)); err != nil {
		t.Errorf("the API server refused an eventNote-truncated note: %v — "+
			"this is the event an operator loses", err)
	}
}

// TestAnUnschedulableProxyPodsEventStaysWithinTheNoteLimit is the wiring test.
//
// The three tests above prove eventNote is correct; none of them proves any
// controller calls it, and a correct helper nobody reaches would leave the
// regression exactly where it was. This drives a real call site --
// reportBlockedProxies, whose note carries the scheduler's own explanation, one
// of the five the review named -- with a message far over the limit, and reads
// what the recorder was actually handed.
func TestAnUnschedulableProxyPodsEventStaysWithinTheNoteLimit(t *testing.T) {
	rec := events.NewFakeRecorder(10)
	r := &ProxyGroupReconciler{Recorder: rec}
	group := &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "spawnery-test"},
	}
	// What a scheduler says on a large cluster: one clause per node pool.
	scheduler := strings.Repeat("0/240 nodes are available: 240 node(s) didn't have free ports; ", 40)
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-proxy-abcde", Namespace: "spawnery-test"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  "Unschedulable",
			Message: scheduler,
		}}},
	}}

	r.reportBlockedProxies(group, pods)

	var got string
	select {
	case got = <-rec.Events:
	default:
		t.Fatal("no event was recorded")
	}
	// FakeRecorder emits "<type> <reason> <note>", so the note is what follows
	// the second space -- the part the API server length-checks.
	prefix := "Warning ProxyPodBlocked "
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("event = %q, want it to start %q", got, prefix)
	}
	note := strings.TrimPrefix(got, prefix)
	if len(note) > maxEventNote {
		t.Errorf("note = %d bytes, want <= %d — the API server drops this event and "+
			"the operator never learns why the pod is stuck", len(note), maxEventNote)
	}
	if !strings.HasSuffix(note, eventNoteTruncated) {
		t.Errorf("note = %q, want the truncation marker", note)
	}
	// The condition keeps the whole thing; that is what the marker points at.
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil || !strings.Contains(cond.Message, "0/240 nodes are available") {
		t.Fatalf("Degraded = %+v, want the full scheduler text the event points at", cond)
	}
	if len(cond.Message) <= maxEventNote {
		t.Errorf("the condition is %d bytes, so this test is not exercising truncation", len(cond.Message))
	}
}
