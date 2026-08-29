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

package podspec

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// The two tests in this file are the answer to a question nobody could ask
// before an upgrade: does this build roll every pod in every installation?
//
// DesiredProxyHash and DesiredServerHash are pure functions of what the
// operator renders. The operator rolls a group when the hash it computes stops
// matching the one stamped on the running pods, so *any* change to the render
// path -- a new environment variable, a changed mount, a different security
// context, a constant nudged by one -- moves the hash for every group
// everywhere, and the next operator upgrade performs a rolling changeover of
// every proxy group and every server group in the cluster. For proxies that
// means players moved off their pods; for servers it means worlds stopped and
// restarted.
//
// docs/known-issues.md records this under milestone 4c-2 and offers a manual
// check: diff internal/podspec/ between two builds, and if nothing in the
// pod-render path moved, the digest cannot have moved either. That check is
// sound and nobody runs it, because it has to be remembered at the moment a
// release is being cut, by someone who is thinking about something else. These
// tests ask the same question at the moment the change is written, in CI, on
// the pull request -- and they cannot be fooled by a guess about which files
// belong to the render path, because they run the render itself.
//
// **A failure here is not necessarily a defect.** Rolling every pod is
// sometimes exactly what a change is for. The test's job is to make that a
// decision somebody takes and records, rather than a side effect discovered by
// players. When you have decided, update the constant and say so in the commit
// message: the release that carries it needs to say it too.
//
// The fixtures below are deliberately frozen literals rather than the package's
// shared testNetwork/testProxyGroup helpers. A shared fixture that somebody
// edits for an unrelated test would move these digests without a single line of
// render logic changing, and a guard that cries wolf is a guard that gets its
// constant updated without being read.

// goldenProxyDigest is DesiredProxyHash over goldenNetwork/goldenProxyGroup,
// goldenAgentEndpoint and goldenConfigValues. See the comment above before
// changing it.
//
// Moved on 2026-08-29, from f08967aaf278196b, and deliberately: the container
// now keeps stdin open so `kubectl attach` can reach the console. The next
// operator upgrade therefore rolls every proxy group in every installation
// once, moving players off their pods, and every server group with them. That
// is the price of the console being reachable at all -- before this, /cloud
// could only be used by granting a permission to a player, which an operator
// bringing a network up for the first time has nobody to grant to.
const goldenProxyDigest = "c52b89c65d114de2"

// goldenServerDigest is DesiredServerHash over goldenNetwork/goldenServerGroup
// and goldenConfigValues. See the comment above before changing it.
//
// Moved on 2026-08-29, from 67445461adf39969, for the reason goldenProxyDigest
// gives. For servers a roll means worlds stopped and restarted, and players
// finishing their sessions on pods being replaced.
const goldenServerDigest = "85fe4733710a4013"

// goldenAgentEndpoint is an input to DesiredProxyHash and not to
// DesiredServerHash -- the asymmetry is deliberate and DesiredServerHash's own
// comment explains it. Frozen here so that neither the operator's real flag
// defaults nor a change to them can reach these digests.
const goldenAgentEndpoint = "spawnery-operator.spawnery-system.svc:9443"

// goldenConfigValues stands in for the marshalled render.Values the controller
// passes. It is a literal rather than a real marshal on purpose: the digest
// takes these bytes verbatim, so a constant input keeps these tests measuring
// the *pod render* alone. The other half -- that the produced bytes themselves
// can move and roll every pod for that reason instead -- belongs to whatever
// produces them, not here.
var goldenConfigValues = []byte("playerLimit: 100\nmotd: golden\n")

func goldenNetwork() *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden-network",
			Namespace: "golden-namespace",
			UID:       "00000000-0000-4000-8000-000000000001",
		},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "golden-forwarding-secret"},
		},
	}
}

func goldenProxyGroup() *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden-proxy-group",
			Namespace: "golden-namespace",
			UID:       "00000000-0000-4000-8000-000000000002",
		},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "golden-network"},
			Replicas:   2,
			Image:      "ghcr.io/spawnery/velocity:golden",
			Expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30000},
			},
			Routing: spawneryv1alpha1.RoutingSpec{FallbackGroups: []string{"golden-lobby"}},
		},
	}
}

func goldenServerGroup() *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden-server-group",
			Namespace: "golden-namespace",
			UID:       "00000000-0000-4000-8000-000000000003",
		},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "golden-network"},
			Type:       spawneryv1alpha1.ServerGroupPersistent,
			Image:      "ghcr.io/spawnery/paper:golden",
			MaxPlayers: 20,
			Replicas:   ptr.To(int32(1)),
			Storage:    &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
		},
	}
}

func TestTheProxyPodDigestHasNotMoved(t *testing.T) {
	got, err := DesiredProxyHash(goldenNetwork(), goldenProxyGroup(), goldenAgentEndpoint, goldenConfigValues)
	if err != nil {
		t.Fatalf("DesiredProxyHash: %v", err)
	}
	if got != goldenProxyDigest {
		t.Errorf(`DesiredProxyHash = %q, want the pinned %q.

Something in the proxy pod render moved. That is not necessarily wrong, but it
is never small: the operator rolls a ProxyGroup when this digest stops matching
the one stamped on its running pods, so the next operator upgrade will perform a
rolling changeover of EVERY ProxyGroup in EVERY installation, moving the players
on them.

If that is what the change is for, update goldenProxyDigest to %[1]q and say so
in the commit message -- the release that carries it has to say it too. If it is
not, this is the accident it exists to catch, and the render change belongs
behind something a group opts into rather than in the shape every group renders.`,
			got, goldenProxyDigest)
	}
}

func TestTheServerPodDigestHasNotMoved(t *testing.T) {
	got, err := DesiredServerHash(goldenNetwork(), goldenServerGroup(), goldenConfigValues)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	if got != goldenServerDigest {
		t.Errorf(`DesiredServerHash = %q, want the pinned %q.

Something in the server pod render moved. The stakes are higher than the proxy
side's: the operator rolls a ServerGroup when this digest stops matching, and a
rolled server is a world stopped and started again, one ordinal at a time, for
every ServerGroup in every installation on the next operator upgrade.

If that is what the change is for, update goldenServerDigest to %[1]q and say so
in the commit message -- the release that carries it has to say it too.`,
			got, goldenServerDigest)
	}
}
