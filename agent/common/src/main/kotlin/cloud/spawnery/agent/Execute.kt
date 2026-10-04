package cloud.spawnery.agent

/** One server's answer to `/cloud execute`. */
data class ExecuteLine(val server: String, val ok: Boolean, val output: List<String>, val error: String)
