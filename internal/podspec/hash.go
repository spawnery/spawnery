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

// DesiredProxyHash digests the pod this operator would render for the group,
// with the pod name held empty so nothing derived from it reaches the digest.
// It takes the group's inputs rather than a built pod to avoid a redaction
// list. The config values are included because motd reaches only the
// ConfigMap; they arrive marshalled so podspec stays free of internal/render.
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
	// LabelPodHash is never set by renderProxyPod, but must not feed back into
	// itself. LabelForwardingHash is set and must go, or rotating the forwarding
	// secret would make every proxy stale at once.
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

// DesiredServerHash is DesiredProxyHash's sibling for one server of the group.
// It takes no *Server, so no per-server identity can enter the digest. The
// config values are included because maxPlayers reaches only the ConfigMap.
// Unlike the proxy hash it excludes the agent endpoint, an operator flag:
// changing it would otherwise restart every world in the installation.
func DesiredServerHash(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ServerGroup,
	configValues []byte,
) (string, error) {
	// A fixed sentinel, because BuildServerPod refuses an empty endpoint.
	subject, err := BuildServerPod(net, group, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Namespace: group.Namespace},
	}, "spawnery.invalid:0")
	if err != nil {
		return "", err
	}
	// See DesiredProxyHash for why these two labels come out.
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

// ForwardingHash digests a Network's forwarding secret for LabelForwardingHash.
// The UID salts it, because pod labels are far more readable than Secrets
// and an unsalted truncated digest invites a shared dictionary attack; the
// zero byte separates the two inputs. Not trimmed: the digest covers exactly
// the bytes the pod mounts.
func ForwardingHash(networkUID types.UID, value []byte) string {
	sum := sha256.New()
	sum.Write([]byte(networkUID))
	sum.Write([]byte{0})
	sum.Write(value)
	return hex.EncodeToString(sum.Sum(nil)[:8])
}
