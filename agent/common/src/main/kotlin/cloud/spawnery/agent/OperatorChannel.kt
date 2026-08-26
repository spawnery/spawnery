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

    /**
     * TLS 1.3 and its three suites, spelled out rather than left to the
     * default.
     *
     * grpc-okhttp picks its ConnectionSpec by platform: the TLS 1.3 + 1.2 one
     * only on Android, and a TLS-1.2-only legacy spec everywhere else,
     * including every JDK. internal/agentserver sets MinVersion:
     * VersionTLS13, so the default leaves the agent offering a version the
     * operator refuses, and the handshake dies with a protocol_version alert
     * before a single byte of HTTP/2 — measured against a real operator-shaped
     * server in hack/agent-test.sh, which is the only level that could see it:
     * the in-process transport used by the unit tests does no TLS at all.
     *
     * Only 1.3, not "1.3 and 1.2": the operator accepts nothing else, so
     * offering 1.2 as well could only ever produce the same failure one round
     * trip later and with a vaguer message.
     */
    private val TLS_VERSIONS = arrayOf("TLSv1.3")
    private val CIPHER_SUITES = arrayOf(
        "TLS_AES_128_GCM_SHA256",
        "TLS_AES_256_GCM_SHA384",
        "TLS_CHACHA20_POLY1305_SHA256",
    )

    /**
     * How long an idle connection waits before the agent pings the operator,
     * and how long it then waits for the answer. Together they are the only
     * clock the agent has on a connection that is up and going nowhere.
     *
     * # Why the agent needs one and the operator does not
     *
     * A node that is hard-powered off, or a network that black-holes, sends no
     * FIN and no RST. Both ends go on believing the connection is fine until
     * TCP's own retransmission gives up, which on a default Linux is minutes:
     * measured 2026-08-26 through a freezable relay at over 200 seconds, and
     * twice not at all within 213.
     *
     * The operator has an application-level answer to that and does not need a
     * transport one. The agent's reports stop the instant the peer does, and
     * `phase.Inputs.AgentSilent` reads "the stream is up and has gone quiet" as
     * exactly what it is, within twice the report interval — ten seconds at the
     * operator's default. A server-side keepalive would be slower than that and
     * would *replace* that state with a weaker one, because breaking the stream
     * turns AgentSilent into an ordinary broken stream, which is tolerated for
     * StreamDownGrace and does not start the drain that moves players off a
     * backend that will never answer. So the operator deliberately sets no
     * keepalive; `internal/agentserver`'s MaxConnectionIdle says so in its own
     * comment.
     *
     * The agent has no such signal. Nothing arrives on a healthy stream between
     * one operator instruction and the next — [SessionLoop] arms every other
     * timer it has off something the operator said — so on a black hole the
     * agent's own sends succeed into a kernel buffer and it learns nothing. The
     * transport ping is the only clock there can be on this side, and unlike
     * the operator's it displaces nothing.
     *
     * # The numbers
     *
     * 45 seconds and 20, so the agent gives up at about 65 rather than at over
     * 200. What bounds them from below is the operator's own enforcement:
     * `MinKeepaliveInterval` is 30 seconds and a client that pings more often
     * than that collects strikes and is sent a GOAWAY. Forty-five leaves half
     * that again as margin for jitter; anything nearer the floor buys seconds
     * and risks the one failure mode this change can introduce, which is an
     * agent the operator refuses for being too eager. hack/agent-test.sh's
     * blackhole phase exercises both directions against a stub carrying the
     * same enforcement policy.
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
            // Only while a call is outstanding, which matches the operator's
            // own KeepaliveEnforcementPolicy: it sets PermitWithoutStream
            // false, so a ping sent between sessions would be a violation
            // rather than a courtesy. The agent holds a stream for all but the
            // moment between one session and the next, so this costs it
            // nothing.
            .keepAliveWithoutCalls(false)
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
