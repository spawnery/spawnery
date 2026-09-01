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
        var a = new ServerInfo("lobby-a3f9", "lobby", ServerPhase.READY, 12, 100, true, "running", Map.of("map", "arena"));
        var b = new ServerInfo("lobby-a3f9", "lobby", ServerPhase.READY, 12, 100, true, "running", Map.of("map", "arena"));
        assertEquals(a, b);
        // Set.copyOf and not Set.of: the latter *throws* on a duplicate, so it
        // would have proved the equality by accident rather than asserted it,
        // and would have failed just as loudly if the records were unequal for
        // some other reason.
        assertEquals(1, Set.copyOf(List.of(a, b)).size());
    }

    @Test
    void aPlayerWhoIsOnNoServerSaysSoWithAnEmptyOptional() {
        var p = new CloudPlayer(UUID.randomUUID(), "someone", Optional.empty());
        assertTrue(p.server().isEmpty());
    }

    // A record with a null component is a record that hands every caller an
    // NPE at some unrelated line later. The compact constructors refuse it at
    // the point of construction, where the stack trace still names the cause.
    @Test
    void aNullComponentIsRefusedWhereItIsBuilt() {
        assertThrows(NullPointerException.class,
                () -> new ServerInfo(null, "lobby", ServerPhase.READY, 0, 100, false, "", Map.of()));
        assertThrows(NullPointerException.class,
                () -> new CloudPlayer(UUID.randomUUID(), "someone", null));
        assertThrows(NullPointerException.class,
                () -> new Group(null, Group.Kind.EPHEMERAL, 1, 1, 0, 100));
    }

    // An unknown phase is not an error and must not throw: the operator may
    // publish a phase this jar predates, and a plugin that crashed on one
    // would break on an operator upgrade it had nothing to do with.
    @Test
    void anUnknownPhaseBecomesUnknownRatherThanAnException() {
        assertEquals(ServerPhase.UNKNOWN, ServerPhase.fromWire("SomethingLaterInvented"));
        assertEquals(ServerPhase.READY, ServerPhase.fromWire("Ready"));
    }

    // freeSlots is never negative: a report can show more players than slots
    // while a group's maxPlayers is being lowered, and a plugin dividing by it
    // or sizing a list from it should not meet a negative number.
    @Test
    void freeSlotsNeverGoesBelowZero() {
        var over = new ServerInfo("lobby-a3f9", "lobby", ServerPhase.READY, 120, 100, true, "", Map.of());
        assertEquals(0, over.freeSlots());
    }
}
