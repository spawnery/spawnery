package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group

internal const val KEY_SHOWN: Int = 6

internal fun shortName(subject: String, group: String, kind: Group.Kind): String {
    if (kind != Group.Kind.ON_DEMAND) return subject
    val prefix = "$group-"
    if (!subject.startsWith(prefix)) return subject
    val key = subject.removePrefix(prefix)
    return if (key.length > KEY_SHOWN) prefix + key.take(KEY_SHOWN) else subject
}
