package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spawnery/spawnery/internal/testenv"
)

func taintedNode(unschedulable bool, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: unschedulable, Taints: taints}}
}

func TestIsDeparting(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		keys []string
		want bool
	}{
		{
			name: "a plain node is not departing",
			node: taintedNode(false),
			want: false,
		},
		{
			name: "cordoned, with no taint keys configured",
			node: taintedNode(true),
			want: true,
		},
		{
			name: "a configured key with NoSchedule",
			node: taintedNode(false, corev1.Taint{Key: "k", Effect: corev1.TaintEffectNoSchedule}),
			keys: []string{"k"},
			want: true,
		},
		{
			name: "the same key with NoExecute",
			node: taintedNode(false, corev1.Taint{Key: "k", Effect: corev1.TaintEffectNoExecute}),
			keys: []string{"k"},
			want: true,
		},
		{
			// PreferNoSchedule does not stop the replacement being scheduled
			// back onto this node, so treating it as departing would condemn
			// the replacement too, and the one after that, without end.
			name: "the same key with PreferNoSchedule is not enough",
			node: taintedNode(false, corev1.Taint{Key: "k", Effect: corev1.TaintEffectPreferNoSchedule}),
			keys: []string{"k"},
			want: false,
		},
		{
			name: "an unconfigured key with NoSchedule",
			node: taintedNode(false, corev1.Taint{Key: "other", Effect: corev1.TaintEffectNoSchedule}),
			keys: []string{"k"},
			want: false,
		},
		{
			name: "a taint is enough on its own, uncordoned",
			node: taintedNode(false, corev1.Taint{Key: "b", Effect: corev1.TaintEffectNoSchedule}),
			keys: []string{"a", "b"},
			want: true,
		},
		{
			name: "cordoned outranks a taint list that matches nothing",
			node: taintedNode(true, corev1.Taint{Key: "other", Effect: corev1.TaintEffectNoSchedule}),
			keys: []string{"k"},
			want: true,
		},
		{
			name: "a nil node is not departing",
			node: nil,
			keys: []string{"k"},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDeparting(tc.node, tc.keys); got != tc.want {
				t.Fatalf("IsDeparting() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNodeDeparting(t *testing.T) {
	c, ctx := testenv.Client(t)

	// An unresolvable node is not departing: failing towards "stay" keeps an
	// unreadable Node from emptying a group on the strength of a cache miss.
	if nodeDeparting(ctx, c, "no-such-node", nil) {
		t.Error("an unreadable node must not read as departing")
	}
	// An empty node name is an unscheduled pod, which is on no node at all.
	if nodeDeparting(ctx, c, "", nil) {
		t.Error("an empty node name must not read as departing")
	}

	// A real node, before and after the cordon. Nodes are cluster-scoped, so
	// the name has to be unique across this whole test binary.
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-departing-test"}}
	if err := c.Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, node) })

	if nodeDeparting(ctx, c, node.Name, nil) {
		t.Error("a plain node reads as departing")
	}
	node.Spec.Unschedulable = true
	if err := c.Update(ctx, node); err != nil {
		t.Fatalf("cordon node: %v", err)
	}
	if !nodeDeparting(ctx, c, node.Name, nil) {
		t.Error("a cordoned node does not read as departing")
	}
}

// TestAWellKnownDrainTaintIsNoticedWithoutBeingActedOn is the answer to
// docs/known-issues.md's "nothing in the operator will tell them they missed
// it". An unset -drain-taint and a genuinely quiet cluster look identical from
// inside this operator, and the one thing that tells them apart is a node
// turning up with a taint that plainly means the node is going.
//
// The two halves are asserted together on purpose. Noticing must not become
// acting: a default that moved pods off another project's taint key would
// couple this operator to a vocabulary that project is free to rename, which
// is the coupling the configurable list exists to avoid.
func TestAWellKnownDrainTaintIsNoticedWithoutBeingActedOn(t *testing.T) {
	for key, project := range map[string]string{
		"ToBeDeletedByClusterAutoscaler": "cluster-autoscaler",
		"karpenter.sh/disrupted":         "Karpenter",
	} {
		t.Run(key, func(t *testing.T) {
			node := &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: key, Effect: corev1.TaintEffectNoSchedule},
			}}}

			departing, hint := departingWithHint(node, nil)
			if departing {
				t.Error("the operator acted on another project's taint key without being told to")
			}
			if hint != project {
				t.Errorf("hint = %q, want %q", hint, project)
			}
		})
	}
}

// TestAConfiguredTaintReportsNothingMissing keeps the warning from firing on
// the cluster that did it right. An operator who passed the flag has nothing
// to be told, and a line saying otherwise would be noise on exactly the
// installations that read their logs.
func TestAConfiguredTaintReportsNothingMissing(t *testing.T) {
	node := &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
		{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule},
	}}}

	departing, hint := departingWithHint(node, []string{"ToBeDeletedByClusterAutoscaler"})
	if !departing {
		t.Error("a configured taint key did not make the node departing")
	}
	if hint != "" {
		t.Errorf("hint = %q, want none: nothing is missing", hint)
	}
}

// TestACordonedNodeReportsNothingMissing is the same point by the other route
// into IsDeparting. A cordon is honoured whatever the taints say, so a node
// that is already departing has nothing missing to report even when it also
// carries an unconfigured key -- which is exactly what
// --cordon-node-before-terminating produces.
func TestACordonedNodeReportsNothingMissing(t *testing.T) {
	node := &corev1.Node{Spec: corev1.NodeSpec{
		Unschedulable: true,
		Taints: []corev1.Taint{
			{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule},
		},
	}}

	departing, hint := departingWithHint(node, nil)
	if !departing || hint != "" {
		t.Errorf("departing=%v hint=%q, want true and none", departing, hint)
	}
}

// TestAPreferNoScheduleWellKnownTaintIsNotEvenNoticed keeps the hint on the
// same footing as the decision. IsDeparting ignores PreferNoSchedule because
// it does not stop the scheduler putting a replacement back on the same node;
// a warning about one would send an operator to add a flag that would then
// have to be ignored anyway.
func TestAPreferNoScheduleWellKnownTaintIsNotEvenNoticed(t *testing.T) {
	node := &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
		{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectPreferNoSchedule},
	}}}

	if departing, hint := departingWithHint(node, nil); departing || hint != "" {
		t.Errorf("departing=%v hint=%q, want false and none", departing, hint)
	}
}

// TestTheMissingTaintWarningIsOncePerNode pins the gate rather than the
// wording. nodeDeparting runs for every pod of every group on every reconcile,
// so an ungated warning would be several lines a second for as long as the
// node stood, about something that does not change while it stands.
func TestTheMissingTaintWarningIsOncePerNode(t *testing.T) {
	var o once
	calls := 0
	for i := 0; i < 5; i++ {
		o.Do("node-a", func() { calls++ })
	}
	o.Do("node-b", func() { calls++ })
	if calls != 2 {
		t.Errorf("calls = %d, want 2: one per node, however often it is asked", calls)
	}
}

// TestNodeDrainingSaysWhenTheGroupCannotReplace closes the half of two entries
// that stayed open after the condemnation itself was settled.
//
// A group in create-backoff, or one whose Network is unusable, condemns the
// pods on a departing node and cannot rebuild them. That ruling is sound --
// those players are evicted off the node whatever the group does, so moving
// them beats being kicked with nowhere chosen. What it costs is capacity, and
// both halves of that were on the object separately (NodeDraining: True beside
// Accepted: False or BackingOff: True) while the combination was on neither.
func TestNodeDrainingSaysWhenTheGroupCannotReplace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		blocked blockedReplacement
		want    []string
		absent  string
	}{
		{
			name:    "nothing in the way",
			blocked: blockedReplacement{},
			want:    []string{"node-a"},
			absent:  "cannot build replacements",
		},
		{
			// The unbounded one. It waits for a person, which is the only
			// case worth waking up for.
			name:    "a broken Network",
			blocked: blockedReplacement{Reason: "its Network is missing or not accepted"},
			want:    []string{"node-a", "cannot build replacements", "Network", "until that is fixed"},
		},
		{
			// The bounded one, which needs no action and must not read like
			// the case above.
			name: "create-backoff",
			blocked: blockedReplacement{
				Reason:  "its servers are failing to start and it is backing off",
				Bounded: true,
			},
			want: []string{"node-a", "backing off", "until that clears on its own"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cond := drainingConditionBlocked([]string{"node-a"}, tc.blocked)
			if cond.Status != metav1.ConditionTrue {
				t.Fatalf("status = %s, want True", cond.Status)
			}
			for _, want := range tc.want {
				if !strings.Contains(cond.Message, want) {
					t.Errorf("message %q does not mention %q", cond.Message, want)
				}
			}
			if tc.absent != "" && strings.Contains(cond.Message, tc.absent) {
				t.Errorf("message %q mentions %q with nothing in the way", cond.Message, tc.absent)
			}
		})
	}
}

// TestAGroupWithNoDrainingNodesSaysNothingAboutReplacing keeps the addition
// off the False side. A group with nothing on a departing node has no
// replacement problem to report, whatever else is wrong with it, and a
// condition that mentioned one would be describing a situation that does not
// exist.
func TestAGroupWithNoDrainingNodesSaysNothingAboutReplacing(t *testing.T) {
	cond := drainingConditionBlocked(nil, blockedReplacement{
		Reason: "its Network is missing or not accepted",
	})
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("status = %s, want False", cond.Status)
	}
	if strings.Contains(cond.Message, "replacements") {
		t.Errorf("message %q talks about replacing with no node draining", cond.Message)
	}
}
