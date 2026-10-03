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
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.Map;
import java.util.List;
import java.util.Optional;
import java.util.Set;
import java.util.UUID;
import org.junit.jupiter.api.Test;

class ValueTypesTest {
    @Test
    void twoDescriptionsOfTheSameServerAreEqual() {
        var a = new ServerInfo("lobby-a3f9", "lobby", ServerPhase.READY, 12, 100, true, "running", Map.of("map", "arena"), "pod-1", 1);
        var b = new ServerInfo("lobby-a3f9", "lobby", ServerPhase.READY, 12, 100, true, "running", Map.of("map", "arena"), "pod-1", 1);
        assertEquals(a, b);
        // Set.copyOf and not Set.of, which throws on a duplicate.
        assertEquals(1, Set.copyOf(List.of(a, b)).size());
    }

    @Test
    void aPlayerWhoIsOnNoServerSaysSoWithAnEmptyOptional() {
        var p = new CloudPlayer(UUID.randomUUID(), "someone", Optional.empty());
        assertTrue(p.server().isEmpty());
    }

    @Test
    void aNullComponentIsRefusedWhereItIsBuilt() {
        assertThrows(NullPointerException.class,
                () -> new ServerInfo(null, "lobby", ServerPhase.READY, 0, 100, false, "", Map.of(), "", 0));
        assertThrows(NullPointerException.class,
                () -> new CloudPlayer(UUID.randomUUID(), "someone", null));
        assertThrows(NullPointerException.class,
                () -> new Group(null, Group.Kind.EPHEMERAL, 1, 1, 0, 100, Map.of(), null));
    }

    @Test
    void aGroupWithoutADisplayNameIsDisplayedByItsName() {
        assertEquals("lobby",
                new Group("lobby", Group.Kind.EPHEMERAL, 1, 1, 0, 100, Map.of(), null).displayName());
        assertEquals("lobby",
                new Group("lobby", Group.Kind.EPHEMERAL, 1, 1, 0, 100, Map.of(), "").displayName());
        assertEquals("Bingo-Team",
                new Group("bingo-team", Group.Kind.EPHEMERAL, 1, 1, 0, 100, Map.of(), "Bingo-Team").displayName());
    }

    @Test
    void anUnknownPhaseBecomesUnknownRatherThanAnException() {
        assertEquals(ServerPhase.UNKNOWN, ServerPhase.fromWire("SomethingLaterInvented"));
        assertEquals(ServerPhase.READY, ServerPhase.fromWire("Ready"));
        assertEquals(ServerPhase.FINISHED, ServerPhase.fromWire("Finished"));
    }

    @Test
    void freeSlotsNeverGoesBelowZero() {
        var over = new ServerInfo("lobby-a3f9", "lobby", ServerPhase.READY, 120, 100, true, "", Map.of(), "", 0);
        assertEquals(0, over.freeSlots());
    }

    @Test
    void aServerCarriesTheNumberAPersonReadsItBy() {
        var second = new ServerInfo("hub-pgqg", "hub", ServerPhase.READY, 0, 100, true, "", Map.of(), "pod-2", 2);

        assertEquals(2, second.number());
    }

    @Test
    void aServerNobodyNumberedCarriesZero() {
        var old = new ServerInfo("hub-dvjk", "hub", ServerPhase.READY, 0, 100, true, "", Map.of(), "pod-1", 0);

        assertEquals(0, old.number());
    }
}
