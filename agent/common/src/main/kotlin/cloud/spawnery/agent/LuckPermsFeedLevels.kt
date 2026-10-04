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
import net.luckperms.api.node.NodeType
import net.luckperms.api.node.types.MetaNode
import java.util.UUID
import java.util.concurrent.CompletionStage

object LuckPermsFeedLevels {
    const val META_KEY: String = "spawnery-feed"

    /** Probed for the same reason as [LuckPermsContexts.registerIfPresent]. */
    fun storeIfPresent(): LevelStore? {
        try {
            Class.forName("net.luckperms.api.LuckPermsProvider")
        } catch (_: ClassNotFoundException) {
            return null
        }
        return Store
    }

    private object Store : LevelStore {
        /** [LuckPermsProvider.get] throws until LuckPerms has enabled, and after it failed to. */
        override fun available(): Boolean =
            try {
                LuckPermsProvider.get()
                true
            } catch (_: IllegalStateException) {
                false
            }

        /** The cached meta, so a value set on a group reaches its members. */
        override fun read(player: UUID): String? =
            LuckPermsProvider.get().userManager.getUser(player)?.cachedData?.metaData?.getMetaValue(META_KEY)

        override fun write(player: UUID, value: String): CompletionStage<*> =
            LuckPermsProvider.get().userManager.modifyUser(player) { user ->
                user.data().clear(NodeType.META.predicate { it.metaKey == META_KEY })
                user.data().add(MetaNode.builder(META_KEY, value).build())
            }
    }
}
