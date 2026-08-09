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
import javax.net.ssl.SSLContext
import javax.net.ssl.TrustManagerFactory
import javax.net.ssl.X509TrustManager

private val AUTHORIZATION: Metadata.Key<String> =
    Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER)

/**
 * The channel to the operator. Trust comes from the mounted CA bundle and from
 * nowhere else — no system trust store — because that pinning is what makes the
 * pod's identity claim meaningful in both directions.
 */
object OperatorChannel {
    /**
     * The bundle may hold several concatenated PEMs. The agent channel design
     * keeps that format open so a later CA rotation can run old and new with
     * overlap; parsing only the first certificate would make the agent the one
     * thing that cannot survive such a rotation.
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

    fun build(endpoint: String, caBundlePem: ByteArray): ManagedChannel {
        val trust = trustManager(caBundlePem)
        val context = SSLContext.getInstance("TLS")
        context.init(null, arrayOf(trust), null)
        return OkHttpChannelBuilder.forTarget(endpoint)
            .useTransportSecurity()
            .sslSocketFactory(context.socketFactory)
            .build()
    }
}

/**
 * The bearer header, assembled by hand and therefore exactly:
 * `Authorization: Bearer <token>`, one space.
 *
 * internal/grpcauth/interceptor.go matches that prefix character for character
 * and fails closed on anything else — reporting "no token" rather than "wrong
 * spelling", which is why a mistake here would be expensive to find.
 *
 * Applied per call rather than per channel, which also makes a stale token
 * structurally impossible: every stream reads the file again.
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
