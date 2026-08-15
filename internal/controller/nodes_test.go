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
