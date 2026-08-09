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
    data class Configured(
        val endpoint: String,
        val caBundle: ByteArray,
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

            return Configured(endpoint, Files.readAllBytes(ca), token)
        }
    }
}
