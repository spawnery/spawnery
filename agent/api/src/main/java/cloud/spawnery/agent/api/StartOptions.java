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

import java.util.Optional;

/**
 * What a plugin chooses for one private server it starts; see
 * {@link SpawneryApi#startServer(String, String, StartOptions)}.
 *
 * <p>Immutable. Whatever is not set is the group's value. Quantities are
 * Kubernetes quantities such as {@code "7Gi"} or {@code "500m"}; the
 * builder does not check them, the operator does.
 */
public final class StartOptions {
    private final ServerResources resources;

    private StartOptions(ServerResources resources) {
        this.resources = resources;
    }

    public static Builder builder() {
        return new Builder();
    }

    /** The resources chosen, {@link ServerResources#NONE} when none were. */
    public ServerResources resources() {
        return resources;
    }

    public static final class Builder {
        private String memoryRequest;
        private String memoryLimit;
        private String cpuRequest;
        private String cpuLimit;
        private String heap;

        private Builder() {}

        /** The memory the server's pod requests. */
        public Builder memoryRequest(String quantity) {
            this.memoryRequest = quantity;
            return this;
        }

        /** The memory limit of the server's pod. */
        public Builder memoryLimit(String quantity) {
            this.memoryLimit = quantity;
            return this;
        }

        /** The CPU the server's pod requests. */
        public Builder cpuRequest(String quantity) {
            this.cpuRequest = quantity;
            return this;
        }

        /** The CPU limit of the server's pod. */
        public Builder cpuLimit(String quantity) {
            this.cpuLimit = quantity;
            return this;
        }

        /** The JVM's maximum heap. */
        public Builder heap(String quantity) {
            this.heap = quantity;
            return this;
        }

        public StartOptions build() {
            return new StartOptions(new ServerResources(
                    set(memoryRequest), set(memoryLimit), set(cpuRequest), set(cpuLimit), set(heap)));
        }

        private static Optional<String> set(String quantity) {
            return Optional.ofNullable(quantity).filter(q -> !q.isEmpty());
        }
    }
}
