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
 * The private server a {@link SpawneryApi#startServer} produced.
 *
 * @param name the server's name, composed by the operator from the group and
 *     the key — this is what {@link SpawneryApi#servers()} calls it and what
 *     {@link SpawneryApi#stopServer} takes
 * @param alreadyRunning whether it was already there, which is a success and
 *     not a refusal: what you asked for is the case. It says nothing about
 *     whether the server is ready for players.
 */
public record StartedServer(String name, boolean alreadyRunning) {}
