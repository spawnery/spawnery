plugins {
    `java-library`
    // Both are Gradle's own, which is the reason they are acceptable here: a
    // third-party publishing plugin would enter agent/deps.json, and that
    // lockfile is what makes the nix build reproducible. Neither of these adds
    // a compile dependency, so the emptiness the comment below defends is
    // untouched.
    `maven-publish`
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
    // Both are required by Maven Central, and both are worth having anyway:
    // this module is read far more often than it is called, and a plugin
    // author stepping into `announce` should land in the prose that explains
    // it rather than in a decompiler.
    withSourcesJar()
    withJavadocJar()
}

// **Published, because a plugin has to be able to compile against this.**
//
// To a local directory and not to a registry: the Central Portal takes one
// signed bundle over its own HTTP API rather than a Maven deploy, so what
// Gradle produces here is the repository layout that hack/publish-api.sh zips
// and uploads. Splitting it that way also means a release can be rehearsed --
// `./gradlew :api:publish` needs no credential and reaches nothing.
publishing {
    publications.create<MavenPublication>("api") {
        artifactId = "spawnery-api"
        from(components["java"])
        pom {
            name = "Spawnery plugin API"
            description = "What a Minecraft plugin can ask the Spawnery cloud, from either side of the proxy."
            url = "https://github.com/spawnery/spawnery"
            licenses {
                license {
                    name = "Apache License, Version 2.0"
                    url = "https://www.apache.org/licenses/LICENSE-2.0.txt"
                }
            }
            developers {
                developer {
                    id = "paulwtf"
                    name = "paulwtf"
                    url = "https://github.com/spawnery"
                }
            }
            scm {
                url = "https://github.com/spawnery/spawnery"
                connection = "scm:git:https://github.com/spawnery/spawnery.git"
                developerConnection = "scm:git:ssh://git@github.com/spawnery/spawnery.git"
            }
        }
    }
    repositories.maven {
        name = "staging"
        url = uri(layout.buildDirectory.dir("staging-deploy"))
    }
}

// Nothing signs here, and that is the second thing this file is careful about.
//
// Gradle's signing plugin reads a key through a Bouncy Castle it bundles, and
// measured on a real key it answers "Could not read PGP secret key" for what
// recent GnuPG versions write by default. What that costs is not the failure
// -- it is that the failure arrives from inside a build, about a key format
// the person who generated the key never chose and cannot see. hack/publish-
// api.sh signs the staged files with gpg itself, so the tool that reads the
// key is the one that wrote it.

tasks.test {
    useJUnitPlatform()
    testLogging {
        showStandardStreams = true
    }
}
