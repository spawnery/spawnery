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

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

class SpawneryTest {
    @AfterEach
    void clear() {
        Spawnery.uninstall();
    }

    @Test
    void withNoAgentTheFailureNamesTheRemedy() {
        assertFalse(Spawnery.isAvailable());
        var e = assertThrows(SpawneryUnavailableException.class, Spawnery::api);
        // The two cases have different remedies -- the plugin is missing, or
        // it has not finished enabling -- and a null return could say neither.
        assertTrue(e.getMessage().contains("spawnery"),
                "the message must name the plugin a server owner has to install: " + e.getMessage());
        assertTrue(e.getMessage().contains("enabl"),
                "the message must also name the load-order case: " + e.getMessage());
    }

    @Test
    void onceInstalledTheSameInstanceComesBack() {
        SpawneryApi api = new FakeApi();
        Spawnery.install(api);
        assertTrue(Spawnery.isAvailable());
        assertSame(api, Spawnery.api());
    }

    @Test
    void installingTwiceIsRefusedRatherThanSilentlyWinning() {
        Spawnery.install(new FakeApi());
        assertThrows(IllegalStateException.class, () -> Spawnery.install(new FakeApi()));
    }

    // The refusal above must not have cost the first one its place: half the
    // plugins holding a handle to a dead agent is the failure being avoided.
    @Test
    void aRefusedSecondInstallLeavesTheFirstStanding() {
        SpawneryApi first = new FakeApi();
        Spawnery.install(first);
        assertThrows(IllegalStateException.class, () -> Spawnery.install(new FakeApi()));
        assertSame(first, Spawnery.api());
    }
}
