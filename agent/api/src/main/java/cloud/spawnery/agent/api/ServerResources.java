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

package cloud.spawnery.agent.api;

import java.util.Objects;
import java.util.Optional;

/**
 * The resources of one private server, as quantities in Kubernetes notation.
 * A component is empty where the server runs with its group's value.
 *
 * <p>Quantities come back in the operator's canonical form: a start asked
 * for {@code 7168Mi} reports {@code 7Gi}. Compare them as quantities, not as
 * strings.
 *
 * @param memoryRequest the memory the server's pod requests
 * @param memoryLimit the memory limit of the server's pod
 * @param cpuRequest the CPU the server's pod requests
 * @param cpuLimit the CPU limit of the server's pod
 * @param heap the JVM's maximum heap, empty when it follows the memory limit
 */
public record ServerResources(
        Optional<String> memoryRequest,
        Optional<String> memoryLimit,
        Optional<String> cpuRequest,
        Optional<String> cpuLimit,
        Optional<String> heap) {
    /** No component set. */
    public static final ServerResources NONE = new ServerResources(
            Optional.empty(), Optional.empty(), Optional.empty(), Optional.empty(), Optional.empty());

    public ServerResources {
        Objects.requireNonNull(memoryRequest, "memoryRequest");
        Objects.requireNonNull(memoryLimit, "memoryLimit");
        Objects.requireNonNull(cpuRequest, "cpuRequest");
        Objects.requireNonNull(cpuLimit, "cpuLimit");
        Objects.requireNonNull(heap, "heap");
    }
}
