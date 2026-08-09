plugins {
    // 2.4.10 and not lower: that is the version measured to read the
    // class-file-major-69 Paper jars on the compile classpath. An older Kotlin
    // may not, and the failure is an UnsupportedClassVersionError that names
    // the jar rather than the compiler.
    kotlin("jvm") version "2.4.10"
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

repositories {
    mavenCentral()
}

// The Paper API comes from the pinned Paper bundle, never from a Maven
// repository, so the plugin cannot compile against a different API than the
// server that loads it. nix/paper-agent.nix symlinks packages.paper-repo here
// before the build; a developer running Gradle by hand creates the same link:
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

// The generated protobuf and gRPC stubs live in their own source set. Its
// compile classpath must never contain paperLibraries: those jars are class
// file major 69, and javac 21 fails the moment it has to resolve a class out
// of one. Keeping them apart makes that impossible rather than unlikely.
val proto: SourceSet by sourceSets.creating {
    java.srcDir("src/proto/java")
}

val protoImplementation: Configuration by configurations.getting

// protobuf-java's version tracks protoc's one-for-one (protoc 35.1 generates
// code that calls APIs only present from protobuf-java 4.35.1 on): the
// project unified its per-language version numbers, so the Java artifact's
// "4." prefix is followed by the same X.Y as protoc itself. This must move
// in lockstep with the protobuf package pinned in flake.nix.
dependencies {
    protoImplementation("io.grpc:grpc-api:1.83.1")
    protoImplementation("io.grpc:grpc-protobuf:1.83.1")
    protoImplementation("io.grpc:grpc-stub:1.83.1")
    protoImplementation("com.google.protobuf:protobuf-java:4.35.1")
    // The generated stubs carry @javax.annotation.Generated.
    protoImplementation("javax.annotation:javax.annotation-api:1.3.2")

    implementation(proto.output)
    implementation("io.grpc:grpc-okhttp:1.83.1")
    implementation("io.grpc:grpc-protobuf:1.83.1")
    implementation("io.grpc:grpc-stub:1.83.1")
    implementation("com.google.protobuf:protobuf-java:4.35.1")

    compileOnly(paperLibraries)

    testImplementation(proto.output)
    testImplementation(kotlin("test"))
    // The BOM, not just the aggregate artifact: junit-jupiter alone does not
    // constrain junit-platform-launcher, and Gradle refuses a dependency with
    // no version rather than guessing one.
    testImplementation(platform("org.junit:junit-bom:5.11.4"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    testImplementation("io.grpc:grpc-inprocess:1.83.1")
    testImplementation("io.grpc:grpc-testing:1.83.1")
    testImplementation("org.bouncycastle:bcpkix-jdk18on:1.79")
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

// make image-repro compares two image builds byte for byte, and this jar is
// inside the image. Without these two flags the archive carries build
// timestamps and a filesystem-order entry list, and the comparison fails for
// reasons that have nothing to do with the code.
tasks.withType<AbstractArchiveTask>().configureEach {
    isPreserveFileTimestamps = false
    isReproducibleFileOrder = true
}
