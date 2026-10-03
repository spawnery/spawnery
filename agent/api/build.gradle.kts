plugins {
    `java-library`
    // Gradle's own; a third-party plugin would enter agent/deps.json.
    `maven-publish`
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

repositories {
    mavenCentral()
}

// Nothing but the test framework. Both agent jars relocate every bundled
// dependency under cloud.spawnery.agent.shaded.*, so a dependency's type in a
// public signature here fails a plugin with NoSuchMethodError at the call.
// PackagingInvariantTest holds this.
//
// No Kotlin either: the relocated @kotlin.Metadata leaves a Kotlin compiler
// reading the shipped jar seeing plain Java.
dependencies {
    testImplementation(platform("org.junit:junit-bom:5.11.4"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}

java {
    sourceCompatibility = JavaVersion.VERSION_21
    targetCompatibility = JavaVersion.VERSION_21
    // Both required by Maven Central.
    withSourcesJar()
    withJavadocJar()
}

// To a local directory: the Central Portal takes one signed bundle over its
// own HTTP API, which hack/publish-api.sh zips from this layout and uploads.
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

// Nothing signs here: Gradle's signing plugin cannot read the key format
// recent GnuPG writes by default, so hack/publish-api.sh signs with gpg.

tasks.test {
    useJUnitPlatform()
    testLogging {
        showStandardStreams = true
    }
}
