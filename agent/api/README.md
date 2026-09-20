# cloud.spawnery:spawnery-api

The public plugin API for [Spawnery](https://docs.spawnery.cloud): what a
Paper or Velocity plugin can ask the cloud, with the same calls on both.

```kotlin
dependencies {
    compileOnly("cloud.spawnery:spawnery-api:0.5.0")
}
```

**`compileOnly`, always.** The classes are loaded from the running agent
plugin. A plugin that bundles its own copy puts a second
`cloud.spawnery.agent.api.SpawneryApi` on the server — a different type with
the same name, so the cast at the first call fails with a message about two
classes that look identical.

This file is the artefact's front page on Maven Central and stops here on
purpose. How to write a plugin against the API, what each call promises, and
what it deliberately cannot do are on the documentation site, where they can
be read without a checkout:

- [Writing a plugin](https://docs.spawnery.cloud/plugin-api/) — depending on
  it, the smallest complete plugin, version skew
- [What a plugin can do](https://docs.spawnery.cloud/plugin-api/what-a-plugin-can-do/)
  — moving a player, changing the fleet, closing a door, the event feed
- [Javadoc](https://docs.spawnery.cloud/plugin-api/javadoc/) — every type and
  method

The version is the one the agent inside the game images carries: the same
number, because they are built from the same source. Compile against the
oldest version you mean to support, not the newest.
