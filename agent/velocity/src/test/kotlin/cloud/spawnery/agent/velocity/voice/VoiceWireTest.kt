package cloud.spawnery.agent.velocity.voice

import org.junit.jupiter.api.Assertions.assertArrayEquals
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import java.nio.ByteBuffer
import java.util.UUID

class VoiceWireTest {
    @Test
    fun `a frame yields its cleartext uuid`() {
        val id = UUID.randomUUID()
        val buf = ByteBuffer.allocate(40).put(VoiceWire.MAGIC)
            .putLong(id.mostSignificantBits).putLong(id.leastSignificantBits).put(ByteArray(23)).flip()
        assertEquals(id, frameUuid(buf))
        assertEquals(0, buf.position())
    }

    @Test
    fun `wrong magic or a short packet is no frame`() {
        assertNull(frameUuid(ByteBuffer.wrap(ByteArray(40))))
        assertNull(frameUuid(ByteBuffer.wrap(byteArrayOf(VoiceWire.MAGIC) + ByteArray(15))))
    }

    @Test
    fun `the request carries the compatibility version as its first int`() {
        assertEquals(20, compatibilityVersion(byteArrayOf(0, 0, 0, 20)))
        assertNull(compatibilityVersion(byteArrayOf(0, 0)))
    }

    @Test
    fun `a version 20 secret gets the new port and host and keeps everything else`() {
        val patched = patchSecret(secretV20(24454, ""), "voice.example.test", 25565)
        assertArrayEquals(secretV20(25565, "voice.example.test"), (patched as SecretPatch.Patched).bytes)
    }

    @Test
    fun `an empty host is written as an empty string`() {
        val patched = patchSecret(secretV20(24454, "backend.internal:24454"), "", 30765)
        assertArrayEquals(secretV20(30765, ""), (patched as SecretPatch.Patched).bytes)
    }

    @Test
    fun `a longer tail than version 20's is left untouched`() {
        assertEquals(SecretPatch.Untouched, patchSecret(secretV20(24454, "", byteArrayOf(1, 0, 0, 0, 7)), "", 25565))
    }

    @Test
    fun `a truncated secret is left untouched`() {
        val full = secretV20(24454, "abc")
        assertEquals(SecretPatch.Untouched, patchSecret(full.copyOf(full.size - 2), "", 25565))
    }

    @Test
    fun `a tail byte other than a boolean is left untouched`() {
        assertEquals(SecretPatch.Untouched, patchSecret(secretV20(24454, "", byteArrayOf(5)), "", 25565))
    }
}
