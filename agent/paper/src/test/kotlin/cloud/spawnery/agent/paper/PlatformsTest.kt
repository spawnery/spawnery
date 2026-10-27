package cloud.spawnery.agent.paper

import org.bukkit.Server
import org.bukkit.plugin.Plugin
import org.junit.jupiter.api.Assertions.assertInstanceOf
import org.junit.jupiter.api.Test
import java.lang.reflect.Proxy

class PlatformsTest {
    private val server = Proxy.newProxyInstance(javaClass.classLoader, arrayOf(Server::class.java)) { _, method, _ ->
        error("the platform choice called Server.${method.name}")
    } as Server

    private val plugin = Proxy.newProxyInstance(javaClass.classLoader, arrayOf(Plugin::class.java)) { _, method, _ ->
        if (method.name == "getServer") server else error("the platform choice called Plugin.${method.name}")
    } as Plugin

    private val withoutCanvas = object : ClassLoader(javaClass.classLoader) {
        override fun loadClass(name: String, resolve: Boolean): Class<*> {
            if (name == Platforms.CANVAS_MARKER) throw ClassNotFoundException(name)
            return super.loadClass(name, resolve)
        }
    }

    @Test
    fun `without the Canvas marker the platform is Paper's`() {
        assertInstanceOf(PaperPlatform::class.java, Platforms.detect(plugin, withoutCanvas))
    }
}
