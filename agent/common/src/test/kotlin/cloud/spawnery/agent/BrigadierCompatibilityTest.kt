package cloud.spawnery.agent

import java.io.File
import kotlin.test.Test
import kotlin.test.assertFalse
import kotlin.test.assertTrue

/**
 * The shared module compiles against Paper's Brigadier and runs against
 * Velocity's, and this is what makes that safe rather than lucky.
 *
 * Measured 2026-08-28 against the pinned artifacts: Paper ships
 * brigadier-1.3.10 with 54 classes, Velocity bundles 52, and the difference is
 * exactly `ContextChain` and `ContextChain$Stage` -- both present in Paper and
 * absent from Velocity. Nothing is in Velocity's copy and missing from
 * Paper's.
 *
 * So a class this module compiles cleanly can still fail to load on a proxy,
 * and the only way it can is by referencing one of those two. The build cannot
 * see that; this can.
 *
 * If a Paper bump widens the gap, this test does not grow on its own -- the
 * list below is the measurement and has to be remeasured. That is stated here
 * rather than left as a surprise for whoever bumps Paper.
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
