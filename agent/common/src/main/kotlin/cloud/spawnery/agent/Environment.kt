package cloud.spawnery.agent

import java.nio.file.Files
import java.nio.file.Path

/** Dormant is a normal outcome: the image also runs outside a cluster, as in `make image-test`. */
sealed interface Environment {
    /** Paths, not contents: the kubelet replaces both files in place, and a CA rotation adds a second PEM. */
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
