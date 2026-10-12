package cloud.spawnery.agent.velocity.voice

import java.io.ByteArrayOutputStream
import java.io.DataOutputStream

/**
 * Field order of SVC's SecretPacket at compatibility version 20
 * (bukkit/src/main/java/de/maxhenkel/voicechat/net/SecretPacket.java, toBytes).
 */
internal fun secretV20(port: Int, host: String, tail: ByteArray = byteArrayOf(1)): ByteArray {
    val out = ByteArrayOutputStream()
    DataOutputStream(out).use { d ->
        d.write(ByteArray(16) { it.toByte() })    // secret
        d.writeInt(port)                          // serverPort
        d.writeLong(0x1122334455667788)           // player UUID msb
        d.writeLong(-0x1122334455667788)          // player UUID lsb
        d.writeByte(0)                            // codec
        d.writeInt(1024)                          // mtuSize
        d.writeDouble(48.0)                       // voiceChatDistance
        d.writeInt(1000)                          // keepAlive
        d.writeBoolean(true)                      // groupsEnabled
        val utf = host.toByteArray(Charsets.UTF_8)
        writeVarInt(d, utf.size)
        d.write(utf)                              // voiceHost
        d.write(tail)                             // allowRecording (+ whatever a newer version adds)
    }
    return out.toByteArray()
}

internal fun writeVarInt(d: DataOutputStream, value: Int) {
    var v = value
    while (v and 0x7F.inv() != 0) {
        d.writeByte((v and 0x7F) or 0x80)
        v = v ushr 7
    }
    d.writeByte(v)
}
