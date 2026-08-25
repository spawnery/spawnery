# Copyright The Spawnery Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# The Java runtime the Velocity image ships, jlink'd to the modules Velocity
# and the proxy agent actually resolve. Its own file rather than a shared one
# with nix/paper-jre.nix: the two classpaths are different programs with
# different dependencies, and eleven of these came from a measurement over
# Velocity's fat jar that says nothing about Paper's.
#
# The eleven came from
#
#   jdeps --print-module-deps --ignore-missing-deps --multi-release 25 \
#     -cp <.#velocity-jar>:<.#agents' velocity/spawnery-agent.jar> \
#     <velocity.jar> <spawnery-agent.jar>
#
# with an empty stderr, so nothing was skipped despite the flag.
#
# jdk.zipfs is the twelfth and no static analysis could have produced it, for
# the same reason Paper's list needed it: it arrives through ServiceLoader.
# Velocity booted on the other eleven and died in
# VelocityServer.registerTranslations with
# java.nio.file.ProviderNotFoundException: Provider "jar" not found -- it reads
# its translation bundles out of its own jar through
# FileSystems.newFileSystem. Measured 2026-08-25 by building without it.
#
# Anything else reached that way would fail the same way, so the checks are
# what stand behind this list rather than the derivation: `make agent-test`
# boots this image and drives a real agent session, TLS handshake included,
# and `make velocity-image-test` boots it with a dormant agent. A Velocity or
# agent bump wants the eleven re-derived rather than assumed.
{ jre25_minimal }:

jre25_minimal.override {
  modules = [
    "java.base"
    "java.compiler"
    "java.desktop"
    "java.management"
    "java.naming"
    "java.net.http"
    "java.rmi"
    "java.scripting"
    "java.sql"
    "jdk.jfr"
    "jdk.zipfs"
    "jdk.unsupported"
  ];
}
