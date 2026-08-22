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
     * The claim the operator's own guard is built on, pinned here rather than
     * left as prose in a Go comment.
     *
     * `internal/certs`' `parsableCert` refuses to publish a rotation slot that
     * is anything other than the PEM encoding of one certificate, and the
     * reason recorded for it was "the agent throws for the whole stream rather
     * than skipping the offending block". That was measured and found wrong in
     * both directions: `X509Factory.readOneBlock` steps over stray bytes that
     * do not begin a line with five hyphens, so a good CA followed by plain
     * garbage parses fine — while a five-hyphen run that opens no valid block
     * kills the stream, taking the certificates already read with it.
     *
     * This is the second half, and the half the guard exists for: a *good*
     * `ca.crt` followed by a PEM envelope around something that is not a
     * certificate leaves the agent with no trust store at all, not with the
     * one good CA. A slot like this reaching the bundle is a fleet-wide
     * outage, which is why the operator repairs or clears it rather than
     * publishing it.
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
     * grpc-okhttp chooses its ConnectionSpec by platform, and on every JDK
     * that choice is a TLS-1.2-only one; the 1.3 spec is reserved for Android.
     * internal/agentserver serves with MinVersion: VersionTLS13, so the
     * default leaves the agent unable to reach the operator at all — the
     * handshake dies with a protocol_version alert, and no unit test built on
     * the in-process transport can see it, because that transport does no TLS.
     *
     * The server here offers both versions rather than only 1.3 on purpose: a
     * regression then shows up as "expected TLSv1.3, was TLSv1.2" instead of
     * an SSLHandshakeException whose message names neither side's intent.
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
            // The RPC is only what makes the channel connect. Nothing on the
            // other end speaks HTTP/2, so it cannot survive — but the TLS
            // handshake is complete before gRPC discovers that, and the
            // handshake is the whole subject here.
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
