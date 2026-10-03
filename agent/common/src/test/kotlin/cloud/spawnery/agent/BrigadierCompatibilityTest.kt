package cloud.spawnery.agent

import java.io.File
import kotlin.test.Test
import kotlin.test.assertFalse
import kotlin.test.assertTrue

/**
 * The module compiles against Paper's Brigadier and runs against Velocity's,
 * which lacks `ContextChain` and `ContextChain$Stage`. The list below is the
 * difference between the pinned copies; a Paper bump has to recompute it.
 */
class BrigadierCompatibilityTest {
    private val absentFromVelocity = listOf(
        "com/mojang/brigadier/context/ContextChain",
    )

    @Test
    fun `nothing in this module references a Brigadier class Velocity does not have`() {
        val classes = File("build/classes/kotlin/main").walkTopDown()
            .filter { it.isFile && it.extension == "class" }
            .toList()

        // A scanner that finds nothing passes every assertion after it.
        assertTrue(classes.isNotEmpty(), "no compiled classes found; this test would have passed on an empty set")

        val offenders = mutableListOf<String>()
        for (file in classes) {
            val bytes = file.readBytes().toString(Charsets.ISO_8859_1)
            for (absent in absentFromVelocity) {
                if (bytes.contains(absent)) {
                    offenders += "${file.name} references $absent"
                }
            }
        }

        assertFalse(
            offenders.isNotEmpty(),
            "these would compile against Paper's Brigadier and fail to load on a Velocity proxy:\n  " +
                offenders.joinToString("\n  "),
        )
    }
}
