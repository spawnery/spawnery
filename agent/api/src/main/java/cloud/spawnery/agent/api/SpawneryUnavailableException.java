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
 * Thrown by {@link Spawnery#api()} when no agent has installed one.
 *
 * <p>An exception and not a null return, because the two ways to get here have
 * different remedies and a null could tell them apart for nobody: either the
 * agent plugin is not installed at all, which is a server owner's problem, or
 * it has not finished enabling, which is a plugin load-order problem. The
 * message names both.
 */
public class SpawneryUnavailableException extends IllegalStateException {
    private static final long serialVersionUID = 1L;

    public SpawneryUnavailableException(String message) {
        super(message);
    }
}
