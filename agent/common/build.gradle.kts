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
dependencies {
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
}

// This jar is an input to :paper's shadowJar and therefore reaches the image.
// The same reproducibility argument as in agent/paper/build.gradle.kts applies
// one link earlier: make image-repro compares two image builds byte for byte.
tasks.withType<AbstractArchiveTask>().configureEach {
    isPreserveFileTimestamps = false
    isReproducibleFileOrder = true
}
