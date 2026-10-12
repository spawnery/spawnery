package cloud.spawnery.agent.velocity.voice

import java.io.ByteArrayOutputStream
import java.nio.ByteBuffer
import java.util.UUID

object VoiceWire {
    const val MAGIC: Byte = 0xFF.toByte()
    const val MIN_FRAME = 17

    /** SVC answers this UUID as a ping; the relay drops and counts it. */
    val PING_UUID: UUID = UUID.fromString("58bc9ae9-c7a8-45e4-a11c-efbb67199425")
    const val SECRET_CHANNEL = "voicechat:secret"
    const val REQUEST_SECRET_CHANNEL = "voicechat:request_secret"
    const val PATCHED_VERSION = 20

    internal const val PORT_OFFSET = 16
    internal const val HOST_OFFSET = 54
}

fun frameUuid(packet: ByteBuffer): UUID? {
    val start = packet.position()
    if (packet.remaining() < VoiceWire.MIN_FRAME || packet.get(start) != VoiceWire.MAGIC) return null
    return UUID(packet.getLong(start + 1), packet.getLong(start + 9))
}

fun compatibilityVersion(requestSecret: ByteArray): Int? =
    if (requestSecret.size < 4) null else ByteBuffer.wrap(requestSecret).int

sealed interface SecretPatch {
    class Patched(val bytes: ByteArray) : SecretPatch
    data object Untouched : SecretPatch
}

fun patchSecret(packet: ByteArray, host: String, port: Int): SecretPatch {
    if (packet.size < VoiceWire.HOST_OFFSET + 2) return SecretPatch.Untouched
    val (length, varIntSize) = readVarInt(packet, VoiceWire.HOST_OFFSET) ?: return SecretPatch.Untouched
    val tailAt = VoiceWire.HOST_OFFSET + varIntSize + length
    if (length < 0 || tailAt + 1 != packet.size) return SecretPatch.Untouched
    if (packet[tailAt] != 0.toByte() && packet[tailAt] != 1.toByte()) return SecretPatch.Untouched

    val out = ByteArrayOutputStream(packet.size + host.length)
    out.write(packet, 0, VoiceWire.PORT_OFFSET)
    out.write(ByteBuffer.allocate(4).putInt(port).array())
    out.write(packet, VoiceWire.PORT_OFFSET + 4, VoiceWire.HOST_OFFSET - VoiceWire.PORT_OFFSET - 4)
    val utf = host.toByteArray(Charsets.UTF_8)
    writeVarInt(out, utf.size)
    out.write(utf)
    out.write(packet[tailAt].toInt())
    return SecretPatch.Patched(out.toByteArray())
}

private fun readVarInt(bytes: ByteArray, at: Int): Pair<Int, Int>? {
    var value = 0
    for (i in 0 until 5) {
        if (at + i >= bytes.size) return null
        val b = bytes[at + i].toInt()
        value = value or ((b and 0x7F) shl (7 * i))
        if (b and 0x80 == 0) return value to (i + 1)
    }
    return null
}

private fun writeVarInt(out: ByteArrayOutputStream, value: Int) {
    var v = value
    while (v and 0x7F.inv() != 0) {
        out.write((v and 0x7F) or 0x80)
        v = v ushr 7
    }
    out.write(v)
}
