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
 * What this process is, in the network's own vocabulary.
 *
 * <p>Sealed rather than a single type with a nullable {@code slots}: a proxy
 * has no player capacity of the kind a backend has, and a field that is
 * meaningless on one of two shapes is a field every caller has to remember is
 * meaningless. The two shapes say which one they are by their type, and a
 * plugin that only ever runs on one platform never meets the other.
 *
 * <p>The names are the Kubernetes object names, so what a plugin prints here
 * is what an operator can paste into {@code kubectl}.
 */
public sealed interface Self permits ServerSelf, ProxySelf {
    /** The name of this pod's own {@code Server} or proxy pod. */
    String name();

    /** The {@code ServerGroup} or {@code ProxyGroup} this belongs to. */
    String group();

    /** The namespace, which is also the boundary of everything this API can see. */
    String namespace();
}
