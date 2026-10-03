// Declares the plugin versions once: the Kotlin plugin warns when each
// subproject loads it with its own version, even an identical one.
plugins {
    // 2.4.10 is the oldest Kotlin that reads the class-file-major-69 Paper jars.
    kotlin("jvm") version "2.4.10" apply false
    id("com.gradleup.shadow") version "9.0.0" apply false
}

// nix/agents.nix passes testJava; without it the tests run on Gradle's own JVM.
subprojects {
    tasks.withType<Test>().configureEach {
        providers.gradleProperty("testJava").orNull?.let { executable = it }
    }
}
