plugins {
    kotlin("jvm")
    // For compileOnlyApi; the Kotlin plugin registers `api` but not that.
    `java-library`
    // No shadow plugin: each agent's own shadowJar bundles and relocates this jar.
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

repositories {
    mavenCentral()
}

// No separate source set for the stubs: it would only keep javac 21 away from
// the class-file-major-69 Paper jars, and this project has no platform jar on
// its classpath.
sourceSets.main {
    java.srcDir("src/proto/java")
}

// Brigadier from Paper's artifact set: com.mojang:brigadier is not on Maven
// Central. compileOnly because both platforms ship their own, and a relocated
// third copy would be a distinct type with the same name. Paper's 1.3.10 adds
// ContextChain and ContextChain$Stage over Velocity's; BrigadierCompatibilityTest
// keeps the tree off those two.
val brigadier = fileTree("paper-repo/libraries/com/mojang/brigadier") { include("**/*.jar") }

dependencies {
    compileOnly(brigadier)
    testImplementation(brigadier)

    api(project(":api"))

    // protobuf-java 4.X.Y moves in lockstep with protoc X.Y pinned in flake.nix.
    api("io.grpc:grpc-api:1.83.1")
    api("io.grpc:grpc-protobuf:1.83.1")
    api("io.grpc:grpc-stub:1.83.1")
    api("com.google.protobuf:protobuf-java:4.35.1")
    // Only for the stubs' source-retention @Generated; nothing needs it at runtime.
    compileOnlyApi("javax.annotation:javax.annotation-api:1.3.2")

    // Not grpc-netty: Paper ships its own Netty. See OperatorChannel.
    implementation("io.grpc:grpc-okhttp:1.83.1")

    // Not on the test classpath, so a test proves registerIfPresent's guard.
    compileOnly("net.luckperms:api:5.5")

    testImplementation(kotlin("test"))
    // The BOM versions junit-platform-launcher, which junit-jupiter does not.
    testImplementation(platform("org.junit:junit-bom:5.11.4"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    testImplementation("io.grpc:grpc-inprocess:1.83.1")
    testImplementation("io.grpc:grpc-testing:1.83.1")
    testImplementation("org.bouncycastle:bcpkix-jdk18on:1.79")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_21)
    }
}

java {
    sourceCompatibility = JavaVersion.VERSION_21
    targetCompatibility = JavaVersion.VERSION_21
}

tasks.test {
    useJUnitPlatform()
    // A Nix build log is the only record of this run.
    testLogging {
        showStandardStreams = true
        events("passed", "skipped", "failed")
    }

    // The failures again at the end: Nix quotes only the last ten lines of a log.
    val failures = mutableListOf<String>()
    afterTest(
        KotlinClosure2({ descriptor: TestDescriptor, result: TestResult ->
            if (result.resultType == TestResult.ResultType.FAILURE) {
                failures += "${descriptor.className}.${descriptor.displayName}"
            }
        }),
    )
    afterSuite(
        KotlinClosure2({ descriptor: TestDescriptor, _: TestResult ->
            if (descriptor.parent == null && failures.isNotEmpty()) {
                // Thrown, not logged: a log line lands above Gradle's failure
                // block, outside the lines Nix quotes.
                throw GradleException(
                    "FAILED TESTS (${failures.size}): " + failures.joinToString("; "),
                )
            }
        }),
    )
}

// Reaches the image through :paper's shadowJar; make image-repro compares builds byte for byte.
tasks.withType<AbstractArchiveTask>().configureEach {
    isPreserveFileTimestamps = false
    isReproducibleFileOrder = true
}
