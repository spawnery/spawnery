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

# The Java runtime the Paper image ships: jlink'd down to the modules Paper and
# the agent actually resolve, rather than the whole headless JDK the image
# carried until 2026-08-25. 405 MiB of closure against jdk25_headless's 697.
#
# Thirteen of these came from
#
#   jdeps --print-module-deps --ignore-missing-deps --multi-release 25 \
#     -cp <all 105 jars of .#paper-repo>:<.#agents' spawnery-agent.jar> \
#     <paper-26.2.jar> <spawnery-agent.jar>
#
# with an empty stderr, so nothing was skipped despite the flag.
#
# jdk.zipfs is the fourteenth, and no static analysis could have produced it.
# Paper booted on the other thirteen and died in Paperclip.extractFiles with
# java.nio.file.ProviderNotFoundException: FileSystems.newFileSystem needs the
# zip provider, which arrives through ServiceLoader rather than through any
# reference jdeps can follow. With it, Paper reaches Done with the agent
# plugin loaded.
#
# So a Paper or agent bump wants this re-derived rather than assumed, and the
# check that covers both halves is `make image-test`, which boots the real
# image. Getting it wrong is a server that will not start, which is loud --
# but it is loud in a container, not in this file.
#
# The derivation itself could not cover the agent's channel -- a boot without an
# operator never opens one, so a security provider reached by name, jdk.crypto.ec
# being the candidate, would have failed the way jdk.zipfs did and only when an
# agent connected. Both checks have since run on this list: `make image-test`
# boots the image and `make agent-test` drives a real session, TLS handshake
# against a rotated CA bundle included. Neither needed a module beyond the
# fourteen.
#
# This list is Paper's. Velocity's is nix/velocity-jre.nix -- a separate
# derivation over a separate classpath, measured the same way and sharing
# exactly one finding with this one: jdk.zipfs, which neither jdeps run could
# see and which both programs die without.
{ jre25_minimal }:

jre25_minimal.override {
  modules = [
    "java.base"
    "java.compiler"
    "java.desktop"
    "java.instrument"
    "java.rmi"
    "java.scripting"
    "java.security.jgss"
    "java.sql"
    "jdk.httpserver"
    "jdk.jfr"
    "jdk.management"
    "jdk.security.auth"
    "jdk.unsupported"
    "jdk.zipfs"
  ];
}
