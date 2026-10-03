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
package cloud.spawnery.agent

import net.luckperms.api.LuckPermsProvider
import net.luckperms.api.context.ImmutableContextSet
import net.luckperms.api.query.QueryOptions
import net.luckperms.api.util.Tristate
import java.util.UUID

object LuckPermsPermissions {
    /** Probed for the same reason as [LuckPermsContexts.registerIfPresent]. */
    fun lookupIfPresent(): ((UUID, String, Map<String, String>) -> PermissionValue?)? {
        try {
            Class.forName("net.luckperms.api.LuckPermsProvider")
        } catch (_: ClassNotFoundException) {
            return null
        }
        return ::value
    }

    private fun value(player: UUID, node: String, contexts: Map<String, String>): PermissionValue? {
        val user = LuckPermsProvider.get().userManager.getUser(player) ?: return null
        val set = ImmutableContextSet.builder().apply { contexts.forEach { (k, v) -> add(k, v) } }.build()
        return when (user.cachedData.getPermissionData(QueryOptions.contextual(set)).checkPermission(node)) {
            Tristate.TRUE -> PermissionValue.TRUE
            Tristate.FALSE -> PermissionValue.FALSE
            else -> PermissionValue.UNDEFINED
        }
    }
}
