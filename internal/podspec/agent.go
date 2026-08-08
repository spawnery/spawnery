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

// These names are shared between the operator's gRPC authenticator
// (internal/grpcauth), which turns an agent's bearer token into a pod
// identity, and the pod specs this package builds, which is what puts the
// matching ServiceAccount and CA bundle onto every managed pod in the first
// place. They live here rather than in grpcauth because grpcauth already
// imports podspec for LabelRole; putting them in grpcauth would create an
// import cycle.
const (
	// AgentTokenAudience is the audience every agent's projected service
	// account token must carry. TokenReview refuses tokens without it.
	AgentTokenAudience = "spawnery-operator"
	// ServerServiceAccountName is the identity of every Paper agent pod.
	ServerServiceAccountName = "spawnery-server"
	// ProxyServiceAccountName is the identity of every Velocity agent pod.
	ProxyServiceAccountName = "spawnery-proxy"
	// CAConfigMapName holds the CA certificate agents use to verify the
	// operator's gRPC endpoint.
	CAConfigMapName = "spawnery-ca"
	// CAConfigMapKey is the data key of CAConfigMapName.
	CAConfigMapKey = "ca.crt"
	// AgentServiceName is the Service in front of the operator's gRPC
	// endpoint. It is the single source of the certificate's SANs and of the
	// address the agents dial, so the two cannot drift apart — and a drift
	// between them shows up as a TLS error in a game server pod, far from its
	// cause. The Service manifest in config/deploy/service.yaml is the third
	// place the name appears; internal/rbacaudit asserts it against this
	// constant, because renaming the Service alone would break every dial and
	// every SAN with a fully green test suite.
	AgentServiceName = "spawnery-operator"
)
