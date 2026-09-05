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
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.lang.reflect.Method;
import org.junit.jupiter.api.Test;

class ReadinessHoldTest {
    @Test
    void closeDeclaresNoCheckedException() throws Exception {
        // The whole point of narrowing AutoCloseable: a plugin writes
        // try (var hold = api.holdReadiness("...")) with no catch.
        Method close = ReadinessHold.class.getMethod("close");
        assertEquals(0, close.getExceptionTypes().length,
                "close must not declare a checked exception");
    }

    @Test
    void aHoldCanBeUsedInTryWithResources() {
        FakeApi api = new FakeApi();
        try (ReadinessHold hold = api.holdReadiness("mappings")) {
            assertTrue(api.heldReasons().contains("mappings"));
        }
        assertTrue(api.heldReasons().isEmpty(), "closing releases the hold");
    }

    @Test
    void closingTwiceReleasesOnce() {
        FakeApi api = new FakeApi();
        ReadinessHold hold = api.holdReadiness("mappings");
        hold.close();
        hold.close();
        assertTrue(api.heldReasons().isEmpty());
    }
}
