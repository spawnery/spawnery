# Copyright paul_wtf.
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

# The Velocity image's Java runtime, jlink'd to the modules Velocity and the
# proxy agent resolve. Re-derive on a Velocity or agent bump with
#
#   jdeps --print-module-deps --ignore-missing-deps --multi-release 25 \
#     -cp <.#velocity-jar>:<.#agents' velocity/spawnery-agent.jar> \
#     <velocity.jar> <spawnery-agent.jar>
#
# and check with `make velocity-image-test` and `make agent-test`. jdk.zipfs
# is not in jdeps' answer: Velocity reads its translations through a zip
# FileSystem, a provider loaded through ServiceLoader.
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
