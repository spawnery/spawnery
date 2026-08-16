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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// DesiredProxyHash digests the pod this operator would render for the group
// right now, with the pod's name held at a fixed empty value so nothing
// derived from it can reach the digest. That is stronger than excluding the
// fields a name reaches today — SPAWNERY_PROXY (EnvProxy) is one, reached
// through the container env rather than through ObjectMeta.Name — and a
// second field derived from the name added to renderProxyPod later would
// need no change here, because it never sees the real name to begin with.
//
// It takes the group's inputs rather than a pod because the digest is a
// property of the group's spec, not of any one rendered instance: computing
// it by redacting a name back out of an already-built pod would have to grow
// a new redaction for every field the name reaches, the same denylist this
// function exists to avoid.
//
// encoding/json sorts map keys, so labels, annotations and node selectors
// serialise in a fixed order and the digest does not flap between passes.
//
// The config values are part of the digest because the rendered pod is not
// everything this operator writes for a proxy. playerLimit rides in the pod as
// SPAWNERY_PLAYER_LIMIT, but motd reaches only the ConfigMap -- so until this
// milestone a changed motd made no proxy stale, ordered no rollout, and never
// reached a running proxy at all. Widening the digest changes its value for
// every existing proxy, so the first reconcile after this ships rolls every
// proxy group once, through the ordinary surge-1 path. They arrive as
// marshalled bytes rather than being rendered here for the same reason
// DesiredServerHash's do: podspec stays free of internal/render (see
// configSecretFile's comment).
func DesiredProxyHash(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	agentEndpoint string,
	configValues []byte,
) (string, error) {
	subject, err := renderProxyPod(net, group, "", agentEndpoint)
	if err != nil {
		return "", err
	}
	// Two labels come out before the digest, for two different reasons.
	// LabelPodHash is belt-and-braces: renderProxyPod never sets it, and this
	// keeps the digest right if that ever stops being true rather than feeding
	// the label back into itself. LabelForwardingHash is not belt-and-braces —
	// renderProxyPod does set it, and removing it here is what stops a rotated
	// forwarding secret from making every proxy of the group stale at once.
	// See LabelForwardingHash's own comment.
	delete(subject.Labels, LabelPodHash)
	delete(subject.Labels, LabelForwardingHash)

	encoded, err := json.Marshal(struct {
		Pod    *corev1.Pod `json:"pod"`
		Config []byte      `json:"config"`
	}{Pod: subject, Config: configValues})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8]), nil
}

// DesiredServerHash digests everything this operator would render for one
// server of the group right now: the pod BuildServerPod would produce for it,
// and the config values the group's ConfigMap carries. The signature takes no
// *Server, so no per-server identity — name, ordinal, claim — has anything to
// enter the digest through; the Server passed to BuildServerPod internally
// carries only a zero-value name, which is defence-in-depth rather than the
// reason identity stays out.
//
// It is the sibling of DesiredProxyHash and follows it deliberately -- same
// encoding/json marshal so map keys sort and the digest does not flap, same
// eight-byte digest, same configValues []byte argument arriving unrendered
// because podspec stays free of internal/render (see configSecretFile's
// comment). Read that function's comment for the argument; it applies here
// unchanged.
//
// The config values are part of the digest because a pod-only hash would miss
// maxPlayers entirely: it never reaches the PodSpec, only the ConfigMap the
// pod mounts by name, so changing it would update the ConfigMap while every
// running server kept the old value and nothing reported the gap.
//
// One divergence from the sibling, deliberate: the agent endpoint is not an
// input. It comes from an operator flag rather than from any spec, and
// including it would mean that restarting the operator with a different
// --operator-namespace restarts every world in the installation.
// DesiredProxyHash does take it, which is a real asymmetry between the two: a
// proxy rolled for that reason loses no world, so the argument that forces
// the exclusion here does not reach it.
func DesiredServerHash(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ServerGroup,
	configValues []byte,
) (string, error) {
	// The endpoint is a fixed sentinel rather than the real one, and rather
	// than "": BuildServerPod refuses an empty endpoint outright
	// (internal/podspec/server.go:231). A constant contributes a constant to
	// every digest and so discriminates nothing, which is exactly the intent.
	subject, err := BuildServerPod(net, group, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Namespace: group.Namespace},
	}, "spawnery.invalid:0")
	if err != nil {
		return "", err
	}
	// Two labels come out, for the two reasons DesiredProxyHash gives:
	// LabelPodHash is belt-and-braces because BuildServerPod never sets it,
	// and LabelForwardingHash is not, because BuildServerPod does — removing
	// it here is what stops a rotated forwarding secret from making every
	// server of the group stale at once.
	delete(subject.Labels, LabelPodHash)
	delete(subject.Labels, LabelForwardingHash)

	encoded, err := json.Marshal(struct {
		Pod    *corev1.Pod `json:"pod"`
		Config []byte      `json:"config"`
	}{Pod: subject, Config: configValues})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8]), nil
}

// ForwardingHash digests a Network's forwarding secret for LabelForwardingHash:
// the network's UID, a zero byte, then the secret's bytes, truncated to eight
// bytes of SHA-256 like the two pod digests above.
//
// The UID is a salt. This value becomes a pod label, and read access to pods is
// granted far more freely than read access to Secrets, so an unsalted truncated
// digest of a weakly chosen secret would turn "no access to the Secret" into an
// off-the-shelf dictionary attack with the precomputation shared across every
// installation of this operator. Salting per network forces that work to be
// redone for each one. It does not defeat a targeted attack on a weak secret;
// docs/known-issues.md records that rather than dressing it up.
//
// The zero byte keeps the two inputs from running together: without it,
// ("ab", "c") and ("a", "bc") are one byte sequence.
//
// The value is not trimmed. A trailing newline is a different digest and is
// reported as a rotation, because the digest covers exactly the bytes the pod
// mounts; what Velocity and Paper make of them is theirs to decide.
func ForwardingHash(networkUID types.UID, value []byte) string {
	sum := sha256.New()
	sum.Write([]byte(networkUID))
	sum.Write([]byte{0})
	sum.Write(value)
	return hex.EncodeToString(sum.Sum(nil)[:8])
}
