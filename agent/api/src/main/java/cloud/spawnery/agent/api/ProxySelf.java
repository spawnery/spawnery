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

package cloud.spawnery.agent.api;

/**
 * A Velocity proxy's view of itself.
 *
 * <p>It adds nothing to {@link Self}, and the empty body is the point: a proxy
 * has no capacity of its own that a plugin should read as a backend's slots.
 * The type exists so that {@code self() instanceof ProxySelf} is how a plugin
 * asks which side it is running on, rather than a string comparison.
 */
public non-sealed interface ProxySelf extends Self {
}
