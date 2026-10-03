plugins {
    kotlin("jvm")
    id("com.gradleup.shadow")
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

// nix/agents.nix installs this jar by its file name.
base {
    archivesName = "spawnery-paper-agent"
}

repositories {
    mavenCentral()
}

// The Paper API comes from the pinned Paper bundle, so the plugin compiles
// against the server that loads it. nix/agents.nix links it; by hand:
//
//   ln -sfn "$(nix build .#paper-repo --no-link --print-out-paths)" agent/paper/paper-repo
//
// Paper's own protobuf-java is an unmanaged jar Gradle cannot resolve against
// ours, and the stubs' gencode check fails if it wins classpath order.
val paperLibraries = fileTree("paper-repo/libraries") {
    include("**/*.jar")
    exclude("**/protobuf-java-*.jar")
}

dependencies {
    implementation(project(":common"))

    compileOnly(paperLibraries)

    testImplementation(kotlin("test"))
    // The BOM versions junit-platform-launcher, which junit-jupiter does not.
    testImplementation(platform("org.junit:junit-bom:5.11.4"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    testImplementation(paperLibraries)
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

tasks.processResources {
    filesMatching("paper-plugin.yml") {
        expand("version" to project.version)
    }
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

// make image-repro compares two image builds byte for byte.
tasks.withType<AbstractArchiveTask>().configureEach {
    isPreserveFileTimestamps = false
    isReproducibleFileOrder = true
}

// `test` still builds the plain jar; without a classifier it would overwrite
// the shaded one of the same name in build/libs.
tasks.jar {
    archiveClassifier.set("plain")
}

tasks.shadowJar {
    archiveClassifier.set("")

    // Everything is relocated, not just what conflicts with Paper today.
    // hack/agent-jar-check.sh fails the build on any class outside
    // cloud/spawnery/agent/, so a new package must be added here.
    listOf(
        "io.grpc",
        "io.perfmark",
        "okio",
        "com.squareup.okhttp3",
        "com.google.protobuf",
        "com.google.common",
        "com.google.thirdparty",
        "com.google.gson",
        "com.google.api",
        "com.google.apps",
        "com.google.cloud",
        "com.google.geo",
        "com.google.logging",
        "com.google.longrunning",
        "com.google.rpc",
        "com.google.shopping",
        "com.google.type",
        "com.google.errorprone",
        "com.google.j2objc",
        "javax.annotation",
        "org.jetbrains",
        "org.intellij",
        "org.jspecify",
        "org.codehaus.mojo",
        "android.annotation",
        "kotlin",
    ).forEach { relocate(it, "cloud.spawnery.agent.shaded.$it") }

    mergeServiceFiles()
}

tasks.build { dependsOn(tasks.shadowJar) }
