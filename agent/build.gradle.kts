// The root project builds nothing. It exists to declare the plugin versions
// once.
//
// Both subprojects used to carry `kotlin("jvm") version "2.4.10"` themselves,
// which Gradle accepts and the Kotlin plugin warns about: "The Kotlin Gradle
// plugin was loaded multiple times in different subprojects, which is not
// supported and may break the build" -- it names ':common' and ':paper' and
// says so even when the versions are identical. Declaring the version here
// with `apply false` and applying it without a version in each subproject is
// the resolution the warning itself prescribes, and it has the side benefit
// that the two agents cannot drift onto different Kotlin versions.
plugins {
    // 2.4.10 and not lower: that is the version measured to read the
    // class-file-major-69 Paper jars on the compile classpath. An older Kotlin
    // may not, and the failure is an UnsupportedClassVersionError that names
    // the jar rather than the compiler.
    kotlin("jvm") version "2.4.10" apply false
    id("com.gradleup.shadow") version "9.0.0" apply false
}
