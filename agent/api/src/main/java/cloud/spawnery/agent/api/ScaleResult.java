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

import java.time.Instant;
import java.util.Objects;

/**
 * The pin a {@link SpawneryApi#scale} call created.
 *
 * <p><b>{@code expiresAt} is the operator's clock, not yours</b>; it is good
 * for telling a person when the pin ends.
 *
 * @param replicas the size the group is now held at.
 */
public record ScaleResult(int replicas, Instant expiresAt) {
    public ScaleResult {
        Objects.requireNonNull(expiresAt, "expiresAt");
    }
}
