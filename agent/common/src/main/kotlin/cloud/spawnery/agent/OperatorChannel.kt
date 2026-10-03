package cloud.spawnery.agent

import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.Metadata
import io.grpc.Status
import io.grpc.okhttp.OkHttpChannelBuilder
import java.io.ByteArrayInputStream
import java.security.KeyStore
import java.security.cert.CertificateException
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import java.util.concurrent.Executor
import java.util.concurrent.TimeUnit
import javax.net.ssl.SSLContext
import javax.net.ssl.TrustManagerFactory
import javax.net.ssl.X509TrustManager

private val AUTHORIZATION: Metadata.Key<String> =
    Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER)

/**
 * Trust comes from the mounted CA bundle only, never the system trust store.
 */
object OperatorChannel {
    /**
     * The bundle may hold several concatenated PEMs, so a CA rotation can run
     * old and new with overlap.
     */
    fun trustManager(caBundlePem: ByteArray): X509TrustManager {
        val factory = CertificateFactory.getInstance("X.509")
        val certificates =
            try {
                factory.generateCertificates(ByteArrayInputStream(caBundlePem))
            } catch (e: CertificateException) {
                throw IllegalArgumentException("the CA bundle contains no certificate", e)
            }
        require(certificates.isNotEmpty()) { "the CA bundle contains no certificate" }

        val keyStore = KeyStore.getInstance(KeyStore.getDefaultType())
        keyStore.load(null, null)
        certificates.forEachIndexed { index, certificate ->
            keyStore.setCertificateEntry("ca-$index", certificate as X509Certificate)
        }

        val trustFactory = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm())
        trustFactory.init(keyStore)
        return trustFactory.trustManagers.filterIsInstance<X509TrustManager>().first()
    }

    /**
     * grpc-okhttp offers only TLS 1.2 on a JDK (its TLS 1.3 spec is
     * Android-only), and internal/agentserver requires TLS 1.3. The unit tests'
     * in-process transport does no TLS, so only hack/agent-test.sh sees this.
     */
    private val TLS_VERSIONS = arrayOf("TLSv1.3")
    private val CIPHER_SUITES = arrayOf(
        "TLS_AES_128_GCM_SHA256",
        "TLS_AES_256_GCM_SHA384",
        "TLS_CHACHA20_POLY1305_SHA256",
    )

    /**
     * The only clock the agent has on a connection that is up and going
     * nowhere: a black-holed peer sends no RST and TCP's own retransmission
     * takes minutes. The operator deliberately sets no keepalive, because
     * `phase.Inputs.AgentSilent` already covers its side and a broken stream
     * would weaken that state.
     *
     * 45 s stays well above the operator's `MinKeepaliveInterval` of 30 s, below
     * which a client collects strikes and is sent a GOAWAY.
     */
    private const val KEEPALIVE_SECONDS = 45L
    private const val KEEPALIVE_TIMEOUT_SECONDS = 20L

    fun build(endpoint: String, caBundlePem: ByteArray): ManagedChannel {
        val trust = trustManager(caBundlePem)
        val context = SSLContext.getInstance("TLS")
        context.init(null, arrayOf(trust), null)
        return OkHttpChannelBuilder.forTarget(endpoint)
            .useTransportSecurity()
            .sslSocketFactory(context.socketFactory)
            .tlsConnectionSpec(TLS_VERSIONS, CIPHER_SUITES)
            .keepAliveTime(KEEPALIVE_SECONDS, TimeUnit.SECONDS)
            .keepAliveTimeout(KEEPALIVE_TIMEOUT_SECONDS, TimeUnit.SECONDS)
            // The operator sets PermitWithoutStream false; a ping between
            // sessions would earn a GOAWAY.
            .keepAliveWithoutCalls(false)
            .build()
    }
}

/**
 * Assembled by hand as exactly `Bearer <token>`, one space: internal/grpcauth
 * matches that prefix literally and reports a misspelling as "no token".
 * Applied per call, so every stream reads the token file again.
 */
object BearerCredentials {
    fun of(tokens: TokenSource): CallCredentials = object : CallCredentials() {
        override fun applyRequestMetadata(
            requestInfo: RequestInfo,
            appExecutor: Executor,
            applier: MetadataApplier,
        ) {
            appExecutor.execute {
                try {
                    val headers = Metadata()
                    headers.put(AUTHORIZATION, "Bearer " + tokens.read())
                    applier.apply(headers)
                } catch (e: Exception) {
                    applier.fail(Status.UNAUTHENTICATED.withCause(e))
                }
            }
        }
    }
}
