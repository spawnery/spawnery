package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.Environment
import java.nio.file.Path

/**
 * Everything the proxy agent needs from outside the JVM: what every agent
 * needs, plus the two variables only a proxy is given.
 *
 * This wraps [Environment] rather than extending it. The server agent has no
 * player limit and no fallback list, and a shared type carrying both as
 * nullables would push the "is it set?" question into every caller — including
 * the router, where the answer has to already be yes.
 *
 * Being dormant is a normal outcome and not a failure, exactly as it is for
 * [Environment]: the image is meant to be runnable outside a cluster, and
 * `make image-test` does exactly that. A dormant proxy produces one log line
 * and silence, never a retry loop.
 */
sealed interface ProxyEnvironment {
    /**
     * [base] is carried whole rather than flattened into three fields.
     * OperatorChannel and TokenSource take the endpoint and the two paths off
     * it, so copying them here would be a second place for them to drift —
     * and in particular a second place where somebody could be tempted to read
     * the CA bundle once at startup, which is the thing
     * [Environment.Configured] documents at length that nobody may do.
     */
    data class Configured(
        val base: Environment.Configured,
        val playerLimit: Int,
        val fallbackGroups: List<String>,
    ) : ProxyEnvironment

    data class Dormant(val reason: String) : ProxyEnvironment

    companion object {
        /** internal/podspec.EnvPlayerLimit. */
        const val PLAYER_LIMIT = "SPAWNERY_PLAYER_LIMIT"

        /** internal/podspec.EnvFallbackGroups. */
        const val FALLBACK_GROUPS = "SPAWNERY_FALLBACK_GROUPS"

        /**
         * The check order is deliberate and is asserted by the tests.
         *
         * [Environment] runs first, so a proxy started outside a cluster — an
         * image smoke test, a developer's laptop — reports the plain "not
         * connecting to an operator" reason. Checking the player limit first
         * would make that run complain about a variable nobody was ever going
         * to set, and the one log line hack/velocity-image-test.sh greps for
         * would name the wrong cause.
         *
         * The two proxy variables are checked after it, and both are hard
         * refusals rather than defaulted. internal/podspec always sets both —
         * the player limit from a CRD field with its own non-zero default, the
         * fallback list from a field the CRD marks required with MinItems=1 —
         * so either one missing or empty means the pod was not built by this
         * operator, or was built by a broken one. Defaulting would hide that:
         * a proxy that guessed a player limit would have the registry silently
         * discard every count above it, and one that guessed a fallback list
         * would pass its ready gate and then disconnect every player with "no
         * available server". Dormancy keeps the pod not-ready, which is the
         * condition an operator actually looks at.
         */
        fun from(getenv: (String) -> String?, agentDir: Path): ProxyEnvironment {
            val base = when (val result = Environment.from(getenv, agentDir)) {
                is Environment.Dormant -> return Dormant(result.reason)
                is Environment.Configured -> result
            }

            val rawLimit = getenv(PLAYER_LIMIT)
            // toIntOrNull covers unset, empty, non-numeric and overflowing in
            // one branch, because the reason is the same in all four: the
            // value the operator is required to set is not a player limit.
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

            return Configured(base, playerLimit, fallbackGroups)
        }

        /**
         * The operator builds this value with strings.Join over a list a human
         * wrote in YAML, so both `"lobby,,hub"` and `"lobby, hub"` are things
         * that reach a pod. Trimming and dropping blanks is not tidiness: a
         * group named `""` or `" hub"` matches nothing in the router, and the
         * failure surfaces as "no servers registered" — a message pointing at
         * the registry rather than at the one stray character that caused it.
         */
        private fun split(raw: String?): List<String> =
            raw.orEmpty().split(',').map(String::trim).filter(String::isNotEmpty)

        /**
         * Quotes the offending value into the dormancy reason, and
         * distinguishes unset from empty. The log line is the only thing
         * anyone will have: "SPAWNERY_PLAYER_LIMIT is not a positive number"
         * on its own leaves the reader unable to tell a typo from a variable
         * the pod never received.
         */
        private fun describe(raw: String?): String =
            if (raw == null) "unset" else "was \"$raw\""
    }
}
