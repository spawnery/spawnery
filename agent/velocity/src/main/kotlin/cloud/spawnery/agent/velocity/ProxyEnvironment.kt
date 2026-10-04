package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.Environment
import java.io.IOException
import java.nio.file.Files
import java.nio.file.Path

/**
 * Everything the proxy agent needs from outside the JVM: what every agent
 * needs, plus the two variables only a proxy is given.
 *
 * Wraps [Environment] rather than extending it, so the server agent carries
 * no nullable player limit or fallback list. Dormant is a normal outcome, as
 * for [Environment]: one log line and silence.
 */
sealed interface ProxyEnvironment {
    /** [base] is carried whole; the CA bundle is read from its path per attempt. */
    data class Configured(
        val base: Environment.Configured,
        val playerLimit: Int,
        val fallbackGroups: List<String>,
        val transfer: Transfer? = null,
        val transferOff: String? = null,
    ) : ProxyEnvironment

    class Transfer(val forceAfterSeconds: Long, val secret: ByteArray, val forceGroups: Set<String> = emptySet())

    data class Dormant(val reason: String) : ProxyEnvironment

    companion object {
        /** internal/podspec.EnvPlayerLimit. */
        const val PLAYER_LIMIT = "SPAWNERY_PLAYER_LIMIT"

        /** internal/podspec.EnvFallbackGroups. */
        const val FALLBACK_GROUPS = "SPAWNERY_FALLBACK_GROUPS"

        const val TRANSFER_FORCE_AFTER_SECONDS = "SPAWNERY_TRANSFER_FORCE_AFTER_SECONDS"
        const val FORWARDING_SECRET_FILE = "SPAWNERY_FORWARDING_SECRET_FILE"
        const val TRANSFER_FORCE_GROUPS = "SPAWNERY_TRANSFER_FORCE_GROUPS"

        /**
         * The check order is asserted by the tests. [Environment] runs first,
         * so a proxy outside a cluster reports the plain "not connecting to an
         * operator" reason that hack/velocity-image-test.sh greps for.
         *
         * The two proxy variables are hard refusals rather than defaulted:
         * internal/podspec always sets both, so a missing one means the pod
         * was not built by this operator. Dormancy keeps the pod not-ready.
         */
        fun from(getenv: (String) -> String?, agentDir: Path): ProxyEnvironment {
            val base = when (val result = Environment.from(getenv, agentDir)) {
                is Environment.Dormant -> return Dormant(result.reason)
                is Environment.Configured -> result
            }

            val rawLimit = getenv(PLAYER_LIMIT)
            val playerLimit = rawLimit?.trim()?.toIntOrNull()
            if (playerLimit == null || playerLimit <= 0) {
                return Dormant(
                    "$PLAYER_LIMIT is not a positive number (${describe(rawLimit)}); " +
                        "refusing to report slots the operator did not set",
                )
            }

            val rawGroups = getenv(FALLBACK_GROUPS)
            val fallbackGroups = split(rawGroups)
            if (fallbackGroups.isEmpty()) {
                return Dormant(
                    "$FALLBACK_GROUPS names no group (${describe(rawGroups)}); " +
                        "refusing to route with nowhere to send a player",
                )
            }

            val (transfer, transferOff) = transfer(getenv)
            return Configured(base, playerLimit, fallbackGroups, transfer, transferOff)
        }

        /** A transfer that cannot be set up is switched off, not the proxy. */
        private fun transfer(getenv: (String) -> String?): Pair<Transfer?, String?> {
            fun off(reason: String) = null to reason

            val rawAfter = getenv(TRANSFER_FORCE_AFTER_SECONDS)
            val rawFile = getenv(FORWARDING_SECRET_FILE)
            if (rawAfter == null && rawFile == null) return null to null
            if (rawAfter == null) return off("$FORWARDING_SECRET_FILE is set but $TRANSFER_FORCE_AFTER_SECONDS is not")
            if (rawFile.isNullOrBlank()) return off("$TRANSFER_FORCE_AFTER_SECONDS is set but $FORWARDING_SECRET_FILE is not")

            val after = rawAfter.trim().toLongOrNull()
            if (after == null || after < 0) {
                return off("$TRANSFER_FORCE_AFTER_SECONDS is not a number of seconds (${describe(rawAfter)})")
            }

            val file = Path.of(rawFile)
            // The way Velocity reads forwarding-secret-file: lines joined, not trimmed.
            val secret = try {
                Files.readAllLines(file).joinToString("").toByteArray(Charsets.UTF_8)
            } catch (e: IOException) {
                return off("cannot read $file: $e")
            }
            if (secret.isEmpty()) return off("$file is empty")
            return Transfer(after, secret, split(getenv(TRANSFER_FORCE_GROUPS)).toSet()) to null
        }

        /**
         * Trims and drops blanks: the value comes from a list a human wrote,
         * and a group named `" hub"` would match nothing in the router.
         */
        private fun split(raw: String?): List<String> =
            raw.orEmpty().split(',').map(String::trim).filter(String::isNotEmpty)

        /** Distinguishes unset from empty in the dormancy reason. */
        private fun describe(raw: String?): String =
            if (raw == null) "unset" else "was \"$raw\""
    }
}
