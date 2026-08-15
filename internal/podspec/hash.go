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
func DesiredProxyHash(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	agentEndpoint string,
) (string, error) {
	subject, err := renderProxyPod(net, group, "", agentEndpoint)
	if err != nil {
		return "", err
	}
	// Belt-and-braces: renderProxyPod never sets LabelPodHash, so this is
	// never actually present. Kept so this function still digests the right
	// thing if that ever stops being true, rather than silently feeding the
	// label back into itself.
	delete(subject.Labels, LabelPodHash)

	encoded, err := json.Marshal(subject)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8]), nil
}
