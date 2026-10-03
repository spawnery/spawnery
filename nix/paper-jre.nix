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

# The Paper image's Java runtime, jlink'd to the modules Paper and the agent
# resolve. Re-derive on a Paper or agent bump with
#
#   jdeps --print-module-deps --ignore-missing-deps --multi-release 25 \
#     -cp <all jars of .#paper-repo>:<.#agents' spawnery-agent.jar> \
#     <paper.jar> <spawnery-agent.jar>
#
# and check with `make image-test` and `make agent-test`. Two modules no
# jdeps run can find: jdk.zipfs (Paperclip loads the zip provider through
# ServiceLoader) and java.net.http (plugins use it, and java.* cannot come
# from a plugin's classpath).
{ jre25_minimal }:

jre25_minimal.override {
  modules = [
    "java.base"
    "java.compiler"
    "java.desktop"
    "java.instrument"
    "java.net.http"
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
