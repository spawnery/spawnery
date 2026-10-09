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

/**
 * What a {@link SpawneryApi#restoreWorld} did.
 *
 * @param generation the world's new current generation, which holds the
 *     restored content; the generation that was current before stays a
 *     restore point
 * @param restoredFrom the generation whose content it holds
 */
public record RestoredWorld(long generation, long restoredFrom) {}
