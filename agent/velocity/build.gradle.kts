plugins {
    // Versions are pinned in agent/build.gradle.kts.
    kotlin("jvm")
    id("com.gradleup.shadow")
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

// nix/agents.nix installs this jar by its file name.
base {
    archivesName = "spawnery-velocity-agent"
}

repositories {
    mavenCentral()
}

// The Velocity API comes from the pinned proxy jar, never from a Maven
// repository, so the plugin cannot compile against a different API than the
// proxy that loads it; the fat jar contains its own plugin API. nix/agents.nix
// symlinks it in as velocity.jar before the build; by hand:
//
//   ln -sfn "$(nix build .#velocity-jar --no-link --print-out-paths)" agent/velocity/velocity.jar
//
// Unlike agent/paper, no `exclude` filter (this jar carries no protobuf, gRPC,
// okhttp or Kotlin) and no separate source set (its classes are Java 21).
val velocityJar = files("velocity.jar")

// The generated stubs arrive through :common's `api` configuration.
dependencies {
    implementation(project(":common"))

    compileOnly(velocityJar)

    testImplementation(kotlin("test"))
    // The BOM versions junit-platform-launcher, which junit-jupiter does not.
    testImplementation(platform("org.junit:junit-bom:5.11.4"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    // Needed as soon as a test names a class whose signature uses Velocity.
    testImplementation(velocityJar)
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

// The version in velocity-plugin.json is what the agent reports as
// Hello.version. Written by hand: Velocity's annotation processor would need
// kapt.
tasks.processResources {
    filesMatching("velocity-plugin.json") {
        expand("version" to project.version)
    }
}

tasks.test {
    useJUnitPlatform()
    testLogging {
        showStandardStreams = true
        events("passed", "skipped", "failed")
    }

    // Failed test names again at the end: Nix quotes only the last ten lines
    // of a failed build.
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
                // Thrown, not logged: a log line lands before Gradle's failure
                // block and outside those ten lines.
                throw GradleException(
                    "FAILED TESTS (${failures.size}): " + failures.joinToString("; "),
                )
            }
        }),
    )
}

// make image-repro compares image builds byte for byte.
tasks.withType<AbstractArchiveTask>().configureEach {
    isPreserveFileTimestamps = false
    isReproducibleFileOrder = true
}

// `test` still builds the plain jar; without a classifier it would overwrite
// the shaded one under the same name.
tasks.jar {
    archiveClassifier.set("plain")
}

tasks.shadowJar {
    archiveClassifier.set("")

    // Kept identical to agent/paper's list. hack/agent-jar-check.sh enforces
    // the rule: it fails on any class outside cloud/spawnery/agent/.
    //
    // Not here and must not be: org.slf4j, com.velocitypowered and
    // com.google.inject. Velocity injects its Logger and instantiates the
    // plugin through Guice; relocation would rewrite the plugin's references to
    // classes no proxy has.
    listOf(
        // gRPC and its transport.
        "io.grpc",
        "io.perfmark",
        "okio",
        "com.squareup.okhttp3",
        // protobuf, guava (both of its top-level packages), and the
        // proto-google-common-protos that grpc-protobuf drags in.
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
        // Annotation-only artifacts.
        "com.google.errorprone",
        "com.google.j2objc",
        "javax.annotation",
        "org.jetbrains",
        "org.intellij",
        "org.jspecify",
        "org.codehaus.mojo",
        "android.annotation",
        // The Kotlin standard library.
        "kotlin",
    ).forEach { relocate(it, "cloud.spawnery.agent.shaded.$it") }

    mergeServiceFiles()
}

tasks.build { dependsOn(tasks.shadowJar) }
