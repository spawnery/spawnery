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

// Shared with internal/grpcauth, which already imports podspec; defining them
// there would create an import cycle.
const (
	// AgentTokenAudience is the audience TokenReview requires on agent tokens.
	AgentTokenAudience       = "spawnery-operator"
	ServerServiceAccountName = "spawnery-server"
	ProxyServiceAccountName  = "spawnery-proxy"
	CAConfigMapName          = "spawnery-ca"
	CAConfigMapKey           = "ca.crt"
	// AgentServiceName is the single source of the certificate's SANs and of the
	// address agents dial. internal/rbacaudit checks config/deploy/service.yaml
	// against it.
	AgentServiceName = "spawnery-operator"
)
