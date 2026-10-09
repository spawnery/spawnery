/*
Copyright paul_wtf.

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

import static org.junit.jupiter.api.Assertions.assertEquals;

import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Map;
import java.util.OptionalDouble;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionStage;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.Test;

class RecordCompatibilityTest {
    private static final ResourceUsage NONE = new ResourceUsage(0, 0, 0, false, 0, 0, 0, false, 0, 0);

    @Test
    void theZeroNinePreviousConstructorsStillBuildAndReadAsUnscheduled() {
        ServerInfo server = new ServerInfo("lobby-a", "lobby", ServerPhase.READY, 1, 20, true, "", Map.of(), "", 0, false);
        assertEquals("", server.node());
        ProxyInfo proxy = new ProxyInfo("gateway-a", "gateway", true, false, 3);
        assertEquals("", proxy.node());
        InstanceStatus instance = new InstanceStatus("lobby-a", "lobby", false, "Ready", true, 1, 20,
                OptionalDouble.empty(), OptionalDouble.empty(), Duration.ZERO, false, false, false, NONE);
        assertEquals("", instance.node());
    }

    @Test
    void aNullNodeReadsAsEmpty() {
        assertEquals("", new ProxyInfo("gateway-a", "gateway", true, false, 3, null).node());
    }

    @Test
    void theZeroTwelveConstructorsStillBuildAndReadEverySeatAsPlayable() {
        ServerInfo server = new ServerInfo("lobby-a", "lobby", ServerPhase.READY, 1, 20, true, "", Map.of(), "", 0, false, "node-1");
        assertEquals(20, server.playableSlots());
        InstanceStatus instance = new InstanceStatus("lobby-a", "lobby", false, "Ready", true, 1, 20,
                OptionalDouble.empty(), OptionalDouble.empty(), Duration.ZERO, false, false, false, NONE, "node-1");
        assertEquals(20, instance.playableSlots());
    }

    @Test
    void aPlayableFigureOutsideOneToSlotsReadsAsEverySeat() {
        assertEquals(20, new ServerInfo("a", "g", ServerPhase.READY, 1, 20, true, "", Map.of(), "", 0, false, "", 0).playableSlots());
        assertEquals(20, new ServerInfo("a", "g", ServerPhase.READY, 1, 20, true, "", Map.of(), "", 0, false, "", 50).playableSlots());
        assertEquals(12, new ServerInfo("a", "g", ServerPhase.READY, 1, 20, true, "", Map.of(), "", 0, false, "", 12).playableSlots());
    }

    @Test
    void theZeroEighteenGroupConstructorStillBuildsAndReadsAsUnpinned() {
        Group group = new Group("lobby", Group.Kind.EPHEMERAL, 1, 1, 0, 100, Map.of(), "Lobby");
        assertEquals(false, group.pinned());
        assertEquals(0, group.pinnedReplicas());
        assertEquals(null, group.pinnedUntil());
    }

    @Test
    void theZeroTwentyFiveListingStillAnswersWithThePointsAlone() throws Exception {
        List<RestorePoint> points = List.of(new RestorePoint(9, Instant.ofEpochMilli(1_791_460_800_000L), true));
        SpawneryApi api = new FakeApi() {
            @Override
            public CompletionStage<RestorePoints> restorePoints(String group, String key) {
                return CompletableFuture.completedFuture(new RestorePoints(points, true));
            }
        };
        assertEquals(points, api.listRestorePoints("worlds", "c0ffee").toCompletableFuture().get(1, TimeUnit.SECONDS));
    }
}
