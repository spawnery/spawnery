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

import java.util.Objects;

/**
 * One thing that happened in the cloud.
 *
 * <p>These are the facts, one per transition, and not the collapsed summary a
 * player sees in chat — the agent collapses for readability, and you get what
 * it collapsed. The {@code message} is the operator's own sentence, the same
 * one {@code kubectl get events} shows for this event, because both are
 * derived from one call rather than computed twice.
 *
 * @param kind the operator's reason, in UpperCamelCase — {@code
 *     ReadyGatePassed}, {@code PodRejected}. A string and not an enum: the
 *     operator's vocabulary gains values, and an agent older than one must
 *     show it rather than fail to parse the message it arrived in. Match on
 *     the ones you know and pass the rest through.
 * @param subject what the event is about — a server name, or a group name for
 *     an event about a group.
 * @param group the group the subject belongs to, or the subject itself when
 *     the event is about a group. Never empty, so grouping needs no special
 *     case.
 * @param warning whether this is the ordinary case or the one somebody should
 *     look at.
 */
public record CloudEventInfo(
        String kind, String subject, String group, String message, boolean warning) {
    public CloudEventInfo {
        Objects.requireNonNull(kind, "kind");
        Objects.requireNonNull(subject, "subject");
        Objects.requireNonNull(group, "group");
        Objects.requireNonNull(message, "message");
    }
}
