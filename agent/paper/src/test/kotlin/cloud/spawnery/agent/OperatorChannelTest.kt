package cloud.spawnery.agent

import io.grpc.CallOptions
import io.grpc.Metadata
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path
import java.util.concurrent.Executor

class OperatorChannelTest {
    @Test
    fun `accepts every certificate in a multi-PEM bundle`() {
        val first = newTestCa("first")
        val second = newTestCa("second")
        val bundle = first.pem() + second.pem()

        val trust = OperatorChannel.trustManager(bundle)

        val accepted = trust.acceptedIssuers.map { it.subjectX500Principal.name }.toSet()
        assertEquals(setOf("CN=first", "CN=second"), accepted)
    }

    @Test
    fun `rejects a bundle with no certificate in it`() {
        assertThrows(IllegalArgumentException::class.java) {
            OperatorChannel.trustManager("not a certificate".toByteArray())
        }
    }

    @Test
    fun `bearer credentials carry the current token, one space after Bearer`(@TempDir dir: Path) {
        val path = dir.resolve("token")
        Files.writeString(path, "abc")
        val credentials = BearerCredentials.of(TokenSource(path))

        assertEquals("Bearer abc", applyAndRead(credentials))

        Files.writeString(path, "def")
        assertEquals("Bearer def", applyAndRead(credentials))
    }

    private fun applyAndRead(credentials: io.grpc.CallCredentials): String {
        var seen: String? = null
        credentials.applyRequestMetadata(
            object : io.grpc.CallCredentials.RequestInfo() {
                override fun getMethodDescriptor() = throw UnsupportedOperationException()
                override fun getSecurityLevel() = io.grpc.SecurityLevel.PRIVACY_AND_INTEGRITY
                override fun getAuthority() = "operator"
                override fun getTransportAttrs() = io.grpc.Attributes.EMPTY
            },
            Executor { it.run() },
            object : io.grpc.CallCredentials.MetadataApplier() {
                override fun apply(headers: Metadata) {
                    seen = headers.get(
                        Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER),
                    )
                }

                override fun fail(status: io.grpc.Status) = throw AssertionError(status.toString())
            },
        )
        return requireNonNull(seen)
    }

    private fun requireNonNull(value: String?): String = value ?: throw AssertionError("no header applied")
}
