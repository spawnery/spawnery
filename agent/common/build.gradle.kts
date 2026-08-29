plugins {
    // Version pinned once in agent/build.gradle.kts.
    kotlin("jvm")
    // For compileOnlyApi below. The Kotlin JVM plugin brings `java` and
    // registers an `api` configuration of its own, which is why `api` resolves
    // without this line -- but not `compileOnlyApi`, and the failure is a
    // Kotlin script "Unresolved reference" that says nothing about plugins.
    // Applying java-library also means both configurations are the documented
    // ones, rather than one of each.
    `java-library`
    // Deliberately not the shadow plugin. This project produces a plain jar;
    // each agent's own shadowJar is what bundles and relocates it, so a shaded
    // artifact here would either be unused or be shaded twice.
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

repositories {
    mavenCentral()
}

// The generated protobuf and gRPC stubs are an ordinary source directory of
// this project's main source set, and not a source set of their own.
//
// They used to have one. The reason was specific to Paper and does not exist
// here: javac 21 fails the moment it has to resolve a class out of a
// class-file-major-69 jar, and Paper's bundled libraries are all of them, so
// the generated Java had to compile with those jars off its classpath. This
// project depends on no platform at all -- no Paper, no Velocity, nothing but
// gRPC and protobuf -- so there is no such jar to keep away from javac, and a
// source set whose only purpose was to keep one away would just be machinery
// with nothing behind it. The next reader will be tempted to "restore" it;
// this comment is the answer.
sourceSets.main {
    java.srcDir("src/proto/java")
}

// protobuf-java's version tracks protoc's one-for-one (protoc 35.1 generates
// code that calls APIs only present from protobuf-java 4.35.1 on): the
// project unified its per-language version numbers, so the Java artifact's
// "4." prefix is followed by the same X.Y as protoc itself. This must move
// in lockstep with the protobuf package pinned in flake.nix.
//
// api rather than implementation for the stub artifacts: :paper (and later
// :velocity) names the generated message types in its own sources, so those
// types have to reach its compile classpath and not merely its runtime one.
// Brigadier, from the copy Paper's own artifact set already carries.
//
// A fileTree and not a version in a path, so a Paper bump moves it without
// this line. Not a Maven coordinate either: com.mojang:brigadier is not on
// Maven Central, and adding a repository for one jar both platforms already
// ship would be a build change out of all proportion.
//
// **compileOnly, and that is load-bearing.** Both platforms provide their own
// at runtime -- Paper as a library, Velocity bundled -- and bundling a third
// would put it through shadowJar's relocation, where the platform's Brigadier
// and the plugin's would be two unrelated types with one name.
//
// Compiled against Paper's 1.3.10, which is a strict superset of Velocity's:
// measured 2026-08-28, 54 classes against 52, and the two extra are
// ContextChain and ContextChain$Stage. Nothing is in Velocity's copy and
// missing from Paper's. CloudCommandCompatibilityTest is what keeps the tree
// out of those two.
val brigadier = fileTree("paper-repo/libraries/com/mojang/brigadier") { include("**/*.jar") }

dependencies {
    compileOnly(brigadier)
    testImplementation(brigadier)

    // `api` and not `implementation`: the module's types appear in signatures
    // :paper and :velocity will implement, so both need it on their compile
    // classpath, and both shadowJars need it on their runtime one. This is
    // also what carries it into the shipped jars at all -- nothing else
    // references it yet, and a module nothing depends on is a module the
    // shaded jars do not carry.
    api(project(":api"))

    api("io.grpc:grpc-api:1.83.1")
    api("io.grpc:grpc-protobuf:1.83.1")
    api("io.grpc:grpc-stub:1.83.1")
    api("com.google.protobuf:protobuf-java:4.35.1")
    // The generated stubs carry @javax.annotation.Generated, and
    // compileOnlyApi rather than api because that is all the annotation is for.
    // It is a source-retention annotation on generated code: javac needs it to
    // compile the stubs, a consumer needs it on its compile classpath for the
    // same reason, and nothing needs it at runtime. On `api` it reaches
    // :paper's runtime classpath, shadowJar bundles it, and the jar grows by 23
    // entries -- 15 classes plus their package docs, a META-INF/maven tree and
    // a licence -- that the single-project build never shipped, because there
    // the artifact sat on `protoImplementation` and never reached a runtime
    // classpath at all. Measured, not estimated: 6694 entries with `api`, 6671
    // with this, against 6669 for the jar before the split. The other four stay
    // `api` -- those really are in the jar, and always were.
    compileOnlyApi("javax.annotation:javax.annotation-api:1.3.2")

    // The transport, and never grpc-netty: Paper ships its own Netty, and the
    // agent must not meet it. See OperatorChannel.
    implementation("io.grpc:grpc-okhttp:1.83.1")

    testImplementation(kotlin("test"))
    // The BOM, not just the aggregate artifact: junit-jupiter alone does not
    // constrain junit-platform-launcher, and Gradle refuses a dependency with
    // no version rather than guessing one.
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

// This jar is an input to :paper's shadowJar and therefore reaches the image.
// The same reproducibility argument as in agent/paper/build.gradle.kts applies
// one link earlier: make image-repro compares two image builds byte for byte.
tasks.withType<AbstractArchiveTask>().configureEach {
    isPreserveFileTimestamps = false
    isReproducibleFileOrder = true
}
