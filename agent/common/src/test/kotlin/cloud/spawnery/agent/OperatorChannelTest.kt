package cloud.spawnery.agent

import cloud.spawnery.agent.pb.AgentServiceGrpc
import cloud.spawnery.agent.pb.OperatorToServer
import io.grpc.CallOptions
import io.grpc.Metadata
import io.grpc.stub.StreamObserver
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.net.InetAddress
import java.nio.file.Files
import java.nio.file.Path
import java.security.KeyStore
import java.security.cert.Certificate
import java.util.Base64
import java.util.concurrent.CompletableFuture
import java.util.concurrent.Executor
import java.util.concurrent.TimeUnit
import javax.net.ssl.KeyManagerFactory
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLServerSocket
import javax.net.ssl.SSLSocket

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

    /**
     * The claim `internal/certs`' `parsableCert` guards against: a good CA
     * followed by a PEM envelope around a non-certificate leaves the agent
     * with no trust store at all, not with the one good CA.
     */
    @Test
    fun `a PEM envelope around a non-certificate kills the whole bundle`() {
        val good = newTestCa("operator-ca")
        val envelope = (
            "-----BEGIN CERTIFICATE-----\n" +
                Base64.getEncoder().encodeToString("this is not a DER certificate".toByteArray()) +
                "\n-----END CERTIFICATE-----\n"
            ).toByteArray()

        assertThrows(IllegalArgumentException::class.java) {
            OperatorChannel.trustManager(good.pem() + envelope)
        }
    }

    @Test
    fun `rejects an empty bundle`() {
        assertThrows(IllegalArgumentException::class.java) {
            OperatorChannel.trustManager(ByteArray(0))
        }
    }

    /**
     * The server offers 1.2 as well, so a regression reads "expected TLSv1.3,
     * was TLSv1.2" rather than an opaque SSLHandshakeException.
     */
    @Test
    fun `negotiates TLS 1_3, the only version the operator will accept`() {
        val ca = newTestCa("operator-ca")
        val serving = newServingCertificate(ca, "localhost")

        val keyStore = KeyStore.getInstance(KeyStore.getDefaultType())
        keyStore.load(null, null)
        keyStore.setKeyEntry(
            "serving",
            serving.keyPair.private,
            CharArray(0),
            arrayOf<Certificate>(serving.certificate, ca.certificate),
        )
        val keys = KeyManagerFactory.getInstance(KeyManagerFactory.getDefaultAlgorithm())
        keys.init(keyStore, CharArray(0))
        val serverContext = SSLContext.getInstance("TLS")
        serverContext.init(keys.keyManagers, null, null)

        val listener = serverContext.serverSocketFactory
            .createServerSocket(0, 1, InetAddress.getLoopbackAddress()) as SSLServerSocket
        listener.enabledProtocols = arrayOf("TLSv1.2", "TLSv1.3")

        val negotiated = CompletableFuture<String>()
        Thread {
            try {
                (listener.accept() as SSLSocket).use { socket ->
                    socket.startHandshake()
                    negotiated.complete(socket.session.protocol)
                }
            } catch (e: Exception) {
                negotiated.completeExceptionally(e)
            }
        }.apply { isDaemon = true }.start()

        val channel = OperatorChannel.build("localhost:${listener.localPort}", ca.pem())
        try {
            // Nothing on the other end speaks HTTP/2; the RPC only makes the
            // channel connect, and the handshake completes before it fails.
            AgentServiceGrpc.newStub(channel).serverSession(
                object : StreamObserver<OperatorToServer> {
                    override fun onNext(value: OperatorToServer) = Unit
                    override fun onError(t: Throwable) = Unit
                    override fun onCompleted() = Unit
                },
            )
            assertEquals("TLSv1.3", negotiated.get(20, TimeUnit.SECONDS))
        } finally {
            channel.shutdownNow()
            listener.close()
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
