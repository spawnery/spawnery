plugins {
    // Both versions are pinned once in agent/build.gradle.kts, including the
    // reason the Kotlin one may not go lower.
    kotlin("jvm")
    id("com.gradleup.shadow")
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

// Named explicitly rather than taken from the subproject directory, because
// nix/agents.nix installs this jar by its file name: with two agents in one
// build, "the directory it came from" stops being enough to tell them apart in
// build/libs, and a rename of the directory would silently change what the
// derivation looks for.
base {
    archivesName = "spawnery-paper-agent"
}

repositories {
    mavenCentral()
}

// The Paper API comes from the pinned Paper bundle, never from a Maven
// repository, so the plugin cannot compile against a different API than the
// server that loads it. nix/agents.nix symlinks packages.paper-repo here
// before the build -- into this subproject, not the Gradle root, which is what
// keeps the relative path below correct; a developer running Gradle by hand
// creates the same link:
//
//   ln -sfn "$(nix build .#paper-repo --no-link --print-out-paths)" agent/paper/paper-repo
//
// Paper's own bundle carries its own protobuf-java (currently 4.29.0), for
// its own unrelated internal use. That jar is excluded here: it is an
// unmanaged file, invisible to Gradle's version conflict resolution, so if
// left in it sits on the classpath next to the protobuf-java this build
// resolves through Maven (pinned to match protoc's version) and the JVM
// loads whichever wins classpath order — observed as the generated stubs'
// gencode-version check failing at class-init with
// ProtobufRuntimeVersionException. The plugin never calls into Paper's copy;
// excluding it just removes the duplicate.
val paperLibraries = fileTree("paper-repo/libraries") {
    include("**/*.jar")
    exclude("**/protobuf-java-*.jar")
}

// The session machinery and the generated stubs live in :common. The stub
// artifacts reach this project's compile classpath through :common's `api`
// configuration, which is why they are not repeated here; grpc-okhttp is on
// :common's `implementation` and reaches only the runtime classpath, which is
// all shadowJar needs to bundle it.
//
// There is no `proto` source set here any more, and no Java at all: the
// generated stubs compile in :common, against a classpath that has never seen
// Paper's class-file-major-69 jars. See agent/common/build.gradle.kts.
dependencies {
    implementation(project(":common"))

    compileOnly(paperLibraries)

    testImplementation(kotlin("test"))
    // The BOM, not just the aggregate artifact: junit-jupiter alone does not
    // constrain junit-platform-launcher, and Gradle refuses a dependency with
    // no version rather than guessing one.
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
    // The per-test events, not just the streams: a Nix build log is the only
    // record anyone will see of this test run, and "BUILD SUCCESSFUL" alone
    // does not distinguish tests that passed from tests that never ran.
    testLogging {
        showStandardStreams = true
        events("passed", "skipped", "failed")
    }

    // The failures again, at the end. The per-test line above is written where
    // the test ran, which in a Nix build is somewhere in the middle of a log
    // Nix then reports as its last ten lines -- so on 2026-08-27 a Velocity
    // test failed in CI, passed on a re-run of the identical derivation, and
    // its name was not recoverable from anything the run kept. A flake nobody
    // can name is a flake nobody can fix.
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
            // The root suite has no parent, so this runs once per test task.
            if (descriptor.parent == null && failures.isNotEmpty()) {
                // Thrown and not merely logged, and that is the whole point. A
                // logged summary lands before Gradle's own failure block, which
                // is outside the ten lines Nix quotes when it reports a failed
                // derivation -- measured, on the first draft of this. Thrown,
                // the names become the "What went wrong" text, which is inside
                // it.
                throw GradleException(
                    "FAILED TESTS (${failures.size}): " + failures.joinToString("; "),
                )
            }
        }),
    )
}

// make image-repro compares two image builds byte for byte, and this jar is
// inside the image. Without these two flags the archive carries build
// timestamps and a filesystem-order entry list, and the comparison fails for
// reasons that have nothing to do with the code.
tasks.withType<AbstractArchiveTask>().configureEach {
    isPreserveFileTimestamps = false
    isReproducibleFileOrder = true
}

// The plain jar keeps building: this project's own `test` task depends on it
// through the test runtime classpath, regardless of what gradleBuildTask
// installs and of whether anything is shaded. Left with no classifier, its
// output filename would be identical to shadowJar's, and since checkPhase
// invokes `gradle test` after buildPhase already ran shadowJar, it would
// silently overwrite the shaded jar in build/libs with an unrelocated one under
// the same name. Giving it a classifier makes that collision impossible rather
// than order-dependent.
tasks.jar {
    archiveClassifier.set("plain")
}

tasks.shadowJar {
    archiveClassifier.set("")

    // Everything the plugin brings is relocated, without exception. The rule
    // is "relocate all of it" rather than "relocate what currently conflicts",
    // because the second list has to be revisited every time Paper changes a
    // bundled library and nobody will remember to.
    //
    // That was the stated rule and not the actual one: the list below used to
    // hold nine entries and the jar shipped 1 085 unrelocated classes, three of
    // whose packages are in Paper's own libraries tree — com.google.thirdparty
    // (guava's *second* top-level package, one line under com.google.common),
    // com.google.errorprone and com.google.j2objc. Enumerating is what let that
    // happen, so the enumeration is no longer what enforces it:
    // hack/agent-jar-check.sh now fails the build on *any* class outside
    // cloud/spawnery/agent/, whether or not it is named here. Adding a
    // dependency that brings a new package therefore fails at build time with
    // the package named, rather than in a pod months later.
    //
    // :common needs no entry of its own: its classes are already under
    // cloud.spawnery.agent, which is the prefix everything else is relocated
    // into.
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
        // Annotation-only artifacts. They carry no behaviour, which is why the
        // three Paper also ships were survivable rather than fatal; it is not a
        // reason to keep them out of the prefix.
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
