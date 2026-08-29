plugins {
    `java-library`
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

repositories {
    mavenCentral()
}

// **This block stays empty of everything but the test framework, and that is
// the module's whole design.** Both agent jars relocate every bundled
// dependency under cloud.spawnery.agent.shaded.* -- the Kotlin standard
// library included, measured as 1045 classes against 0 at the real
// coordinates. A type from any dependency appearing in a public signature here
// would be a type the shipped jar has moved out from under a plugin compiled
// against the real one, and the symptom is a NoSuchMethodError at the call
// rather than a compile error anywhere. PackagingInvariantTest is what holds
// this; the emptiness here is what makes it easy to hold.
//
// No Kotlin plugin either, for the same measurement: a Kotlin class carries a
// @kotlin.Metadata annotation, that annotation is relocated with everything
// else, and a Kotlin compiler reading the shipped jar then finds no metadata
// and sees plain Java -- no nullability, no default arguments, no data
// classes. Writing this module in Java is what makes what a plugin author
// compiles against the same thing they get.
dependencies {
    testImplementation(platform("org.junit:junit-bom:5.11.4"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}

java {
    sourceCompatibility = JavaVersion.VERSION_21
    targetCompatibility = JavaVersion.VERSION_21
}

tasks.test {
    useJUnitPlatform()
    testLogging {
        showStandardStreams = true
    }
}
