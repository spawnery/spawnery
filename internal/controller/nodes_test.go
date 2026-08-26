package controller

import (
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
