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

import java.util.Objects;

/**
 * One proxy of this network, as the operator last described it.
 *
 * @param name the proxy's pod name
 * @param group the proxy group it belongs to
 * @param ready whether it passes its ready gate
 * @param draining whether it takes no new connections and stops once empty
 * @param players the players its agent reports
 */
public record ProxyInfo(String name, String group, boolean ready, boolean draining, int players) {
    public ProxyInfo {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(group, "group");
    }
}
