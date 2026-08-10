package cloud.spawnery.agent

import java.nio.file.Files
import java.nio.file.Path

/**
 * Everything the agent needs from outside the JVM, and the decision whether it
 * has enough to run at all.
 *
 * Being dormant is a normal outcome, not a failure: the image is meant to be
 * runnable outside a cluster, and make image-test does exactly that. A missing
 * endpoint therefore produces one log line and silence, not a retry loop.
 */
sealed interface Environment {
    /**
     * Paths rather than contents, for both of them and for the same reason.
     *
     * `token` is a projected ServiceAccount token the kubelet replaces in place
     * every ten minutes, and `ca.crt` is a ConfigMap projection it updates the
     * same way. A CA rotation that runs old and new with overlap is exactly
     * what [OperatorChannel.trustManager] parses a multi-PEM bundle for — and a
     * bundle captured at `onEnable` would never hold the second PEM, so the
     * agent would be the one thing in the cluster that cannot survive the
     * rotation its own comment claims to support. Nothing rotates today
     * (internal/certs sets a ten-year CA lifetime), which is why this is a
     * property to keep rather than a bug to fix.
     */
    data class Configured(
        val endpoint: String,
        val caBundlePath: Path,
        val tokenPath: Path,
    ) : Environment

    data class Dormant(val reason: String) : Environment

    companion object {
        const val ENDPOINT = "SPAWNERY_OPERATOR_ENDPOINT"

        fun from(getenv: (String) -> String?, agentDir: Path): Environment {
            val endpoint = getenv(ENDPOINT)
            if (endpoint.isNullOrBlank()) {
                return Dormant("$ENDPOINT is not set; not connecting to an operator")
            }

            val ca = agentDir.resolve("ca.crt")
            if (!Files.isReadable(ca)) {
                return Dormant("$ca is not readable; refusing to trust anything else")
            }

            val token = agentDir.resolve("token")
            if (!Files.isReadable(token)) {
                return Dormant("$token is not readable")
            }

            return Configured(endpoint, ca, token)
        }
    }
}
