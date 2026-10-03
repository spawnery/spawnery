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
 * What this process is, in the network's own vocabulary.
 *
 * <p>A {@link ServerSelf} or a {@link ProxySelf}; only a backend has
 * {@code slots}.
 *
 * <p>The names are the Kubernetes object names, so what a plugin prints here
 * is what an operator can paste into {@code kubectl}.
 */
public sealed interface Self permits ServerSelf, ProxySelf {
    /** The name of this pod's own {@code Server} or proxy pod. */
    String name();

    /** The {@code ServerGroup} or {@code ProxyGroup} this belongs to. */
    String group();

    /**
     * The {@code Network} this belongs to, which is also the boundary of
     * everything this API can see. One namespace holds exactly one
     * {@code Network}.
     */
    String network();
}
