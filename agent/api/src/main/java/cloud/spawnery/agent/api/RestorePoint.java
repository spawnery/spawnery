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
 * One generation of a private server's world that
 * {@link SpawneryApi#restoreWorld} can go back to.
 *
 * @param generation the generation's number; a higher one is newer
 * @param taken when the node took the snapshot, not when it reached the
 *     object store
 * @param current whether this is the world as it stands, which a restore
 *     cannot pick
 */
public record RestorePoint(long generation, Instant taken, boolean current) {
    public RestorePoint {
        Objects.requireNonNull(taken, "taken");
    }
}
