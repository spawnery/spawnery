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

import java.util.Objects;

/**
 * What the operator did with a {@link SpawneryApi#connect} call.
 *
 * <p><b>{@code ordered} is not {@code moved}, and the difference is not
 * pedantry.</b> The proxy that carries a move calls Velocity's
 * {@code connectWithIndication} and does not wait on the future it returns:
 * blocking a network callback on a round trip to a backend is a cost the agent
 * cannot pay, and that decision is what keeps a drain from stalling. So no
 * proxy in this system can report whether a player arrived, and an operator
 * claiming to would be inventing the answer.
 *
 * <p>What to do with that: {@code ordered} says the instruction reached a
 * proxy holding the player. If you need to know they arrived, read
 * {@link SpawneryApi#player} a moment later — the mirror is what carries that,
 * and it is the only honest source.
 *
 * @param alreadyThere the one case where nothing was ordered and nothing is
 *     wrong: the player was on {@code target} when the request arrived.
 */
public record ConnectResult(boolean ordered, boolean alreadyThere, String target) {
    public ConnectResult {
        Objects.requireNonNull(target, "target");
    }
}
