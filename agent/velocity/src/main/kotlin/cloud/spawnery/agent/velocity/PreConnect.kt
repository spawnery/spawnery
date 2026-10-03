package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer

/**
 * What a join rule closing a connect becomes. Velocity 4.2.0 drops a denied
 * initial connect without a fallback, so the player would stay on the proxy
 * with no backend; hence the redirect and the disconnect.
 */
sealed interface PreConnectDecision {
    data object Keep : PreConnectDecision

    data object Deny : PreConnectDecision

    data class Redirect(val server: RegisteredServer) : PreConnectDecision

    data object Disconnect : PreConnectDecision
}

fun decidePreConnect(
    allowed: Boolean,
    onServer: Boolean,
    alternative: () -> RegisteredServer?,
): PreConnectDecision = when {
    allowed -> PreConnectDecision.Keep
    onServer -> PreConnectDecision.Deny
    else -> alternative()?.let { PreConnectDecision.Redirect(it) } ?: PreConnectDecision.Disconnect
}
