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
 * A hold that keeps this server out of readiness until it is released.
 *
 * <p>Narrows {@link AutoCloseable#close()} to throw nothing, so a plugin can
 * write {@code try (var hold = api.holdReadiness("..."))} with no catch, and
 * can equally keep the handle and close it from a callback when its own
 * executor finishes.
 *
 * <p>Closing twice releases once.
 */
public interface ReadinessHold extends AutoCloseable {
    @Override
    void close();
}
