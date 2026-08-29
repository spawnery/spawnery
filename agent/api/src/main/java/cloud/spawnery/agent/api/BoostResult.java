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

import java.time.Instant;
import java.util.Objects;

/**
 * The extra capacity a {@link SpawneryApi#boost} call created.
 *
 * <p><b>{@code expiresAt} is the operator's clock, not yours.</b> The request
 * carries a duration and the answer carries an instant, and the asymmetry is
 * deliberate: the two sides do not share a clock, and an expiry computed on a
 * pod whose clock is minutes fast would end the boost early or late by exactly
 * that error — silently, since neither side can see the difference. The
 * operator's clock is the one the scaler reads, so it is the one that decides.
 *
 * <p>Compare it against {@link Instant#now()} at your own risk for the same
 * reason. What it is good for is telling a person when the extra servers go
 * away.
 *
 * @param replicas how many servers this boost adds. The operator's figure, not
 *     the one you asked for — it refuses rather than trimming, so these agree
 *     today, and a caller that reads this one cannot be wrong if that ever
 *     changes.
 */
public record BoostResult(int replicas, Instant expiresAt) {
    public BoostResult {
        Objects.requireNonNull(expiresAt, "expiresAt");
    }
}
