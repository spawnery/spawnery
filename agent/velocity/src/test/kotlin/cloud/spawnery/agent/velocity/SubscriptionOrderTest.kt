package cloud.spawnery.agent.velocity

import com.velocitypowered.api.event.EventHandler
import com.velocitypowered.api.event.PostOrder
import com.velocitypowered.api.event.Subscribe
import com.velocitypowered.api.event.player.ServerPreConnectEvent
import com.velocitypowered.api.plugin.PluginContainer
import com.velocitypowered.api.plugin.PluginDescription
import com.velocitypowered.api.plugin.PluginManager
import com.velocitypowered.api.proxy.Player
import com.velocitypowered.api.proxy.server.RegisteredServer
import com.velocitypowered.api.proxy.server.ServerInfo
import com.velocitypowered.proxy.event.VelocityEventManager
import org.junit.jupiter.api.Test
import java.lang.reflect.Proxy
import java.net.InetSocketAddress
import java.nio.file.Path
import java.util.Optional
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import kotlin.test.assertEquals

class SubscriptionOrderTest {
    @Test
    fun `the join check and the switch decision are one handler, at the lowest priority`() {
        val handlers = AgentPlugin::class.java.methods.filter {
            it.isAnnotationPresent(Subscribe::class.java) &&
                it.parameterTypes.singleOrNull() == ServerPreConnectEvent::class.java
        }

        assertEquals(listOf("onServerPreConnect"), handlers.map { it.name })
        assertEquals(Short.MIN_VALUE, handlers.single().getAnnotation(Subscribe::class.java).priority)
    }

    /** Velocity's own ordering, which the handler above relies on: PostOrder.LAST is -32767. */
    @Test
    fun `a handler at the lowest priority sees a redirect made at PostOrder LAST, whoever registered first`() {
        for (lastFirst in listOf(true, false)) {
            val executor = Executors.newSingleThreadExecutor()
            try {
                val events = VelocityEventManager(plugins(executor))
                val vip = server("vip-1")
                var seen: String? = null
                val redirect = EventHandler<ServerPreConnectEvent> {
                    it.result = ServerPreConnectEvent.ServerResult.allowed(vip)
                }
                val check = EventHandler<ServerPreConnectEvent> {
                    seen = it.result.server.get().serverInfo.name
                }
                if (lastFirst) {
                    events.register(Any(), ServerPreConnectEvent::class.java, PostOrder.LAST, redirect)
                    events.register(Any(), ServerPreConnectEvent::class.java, Short.MIN_VALUE, check)
                } else {
                    events.register(Any(), ServerPreConnectEvent::class.java, Short.MIN_VALUE, check)
                    events.register(Any(), ServerPreConnectEvent::class.java, PostOrder.LAST, redirect)
                }

                events.fire(ServerPreConnectEvent(player(), server("lobby-1"))).get(5, TimeUnit.SECONDS)

                assertEquals("vip-1", seen, "registered with LAST first: $lastFirst")
            } finally {
                executor.shutdownNow()
            }
        }
    }

    private fun plugins(executor: ExecutorService): PluginManager {
        val container = object : PluginContainer {
            override fun getDescription(): PluginDescription = PluginDescription { "test" }
            override fun getExecutorService(): ExecutorService = executor
        }
        return object : PluginManager {
            override fun fromInstance(instance: Any?): Optional<PluginContainer> = Optional.of(container)
            override fun getPlugin(id: String?): Optional<PluginContainer> = Optional.of(container)
            override fun getPlugins(): Collection<PluginContainer> = listOf(container)
            override fun isLoaded(id: String?): Boolean = true
            override fun addToClasspath(plugin: Any?, path: Path?) = Unit
        }
    }

    private fun server(name: String): RegisteredServer {
        val info = ServerInfo(name, InetSocketAddress.createUnresolved("10.0.0.1", 25565))
        return stub(RegisteredServer::class.java) { method -> if (method == "getServerInfo") info else null }
    }

    private fun player(): Player = stub(Player::class.java) { null }

    private fun <T> stub(type: Class<T>, answer: (String) -> Any?): T =
        type.cast(
            Proxy.newProxyInstance(type.classLoader, arrayOf(type)) { self, method, args ->
                when (method.name) {
                    "toString" -> type.simpleName
                    "hashCode" -> System.identityHashCode(self)
                    "equals" -> self === args?.get(0)
                    else -> answer(method.name) ?: if (method.returnType == Optional::class.java) Optional.empty<Any>() else null
                }
            },
        )
}
