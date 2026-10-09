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

import java.util.List;

/**
 * What a {@link SpawneryApi#restorePoints} found.
 *
 * @param points the world's restore points, the current one first, then the
 *     older ones newest first
 * @param settling whether the world may still change, so that the points
 *     can miss its newest state and a restore is turned away; see
 *     {@link SpawneryApi#restorePoints}
 */
public record RestorePoints(List<RestorePoint> points, boolean settling) {
    public RestorePoints {
        points = List.copyOf(points);
    }
}
