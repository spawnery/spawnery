/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloud.spawnery.agent.api;

/**
 * Where a server is in its life, in the operator's own vocabulary.
 *
 * <p>{@link #UNKNOWN} is not a failure case, it is forward compatibility: the
 * operator may publish a phase this jar predates, and a plugin that threw on
 * one would break on an operator upgrade it had nothing to do with. Use
 * {@link #fromWire(String)} rather than {@code valueOf}, which throws.
 */
public enum ServerPhase {
    PENDING,
    STARTING,
    READY,
    RETIRING,
    DRAINING,
    TERMINATING,
    FAILED,
    UNKNOWN;

    /** Maps the operator's spelling onto this enum, never throwing. */
    public static ServerPhase fromWire(String phase) {
        if (phase == null) {
            return UNKNOWN;
        }
        for (ServerPhase p : values()) {
            if (p != UNKNOWN && p.name().equalsIgnoreCase(phase)) {
                return p;
            }
        }
        return UNKNOWN;
    }
}
