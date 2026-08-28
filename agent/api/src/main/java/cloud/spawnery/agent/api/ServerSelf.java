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

/** A Paper backend's view of itself. */
public non-sealed interface ServerSelf extends Self {
    /**
     * The player capacity this server was configured with -- the group's
     * {@code spec.maxPlayers}, which is also what the agent reports upward.
     */
    int slots();
}
