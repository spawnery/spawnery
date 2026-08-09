package cloud.spawnery.agent

import org.bouncycastle.asn1.x500.X500Name
import org.bouncycastle.cert.jcajce.JcaX509CertificateConverter
import org.bouncycastle.cert.jcajce.JcaX509v3CertificateBuilder
import org.bouncycastle.operator.jcajce.JcaContentSignerBuilder
import java.io.StringWriter
import java.math.BigInteger
import java.security.KeyPair
import java.security.KeyPairGenerator
import java.security.cert.X509Certificate
import java.util.Base64
import java.util.Date

/** A self-signed CA, generated per test so no fixture can expire. */
data class TestCa(val certificate: X509Certificate, val keyPair: KeyPair) {
    fun pem(): ByteArray = pemOf(certificate)
}

fun newTestCa(commonName: String): TestCa {
    val keyPair = KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair()
    val now = System.currentTimeMillis()
    val name = X500Name("CN=$commonName")
    val builder = JcaX509v3CertificateBuilder(
        name,
        BigInteger.valueOf(now),
        Date(now - 60_000),
        Date(now + 3_600_000),
        name,
        keyPair.public,
    )
    val signer = JcaContentSignerBuilder("SHA256withRSA").build(keyPair.private)
    val certificate = JcaX509CertificateConverter().getCertificate(builder.build(signer))
    return TestCa(certificate, keyPair)
}

fun pemOf(certificate: X509Certificate): ByteArray {
    val encoder = Base64.getMimeEncoder(64, "\n".toByteArray())
    val body = encoder.encodeToString(certificate.encoded)
    val out = StringWriter()
    out.write("-----BEGIN CERTIFICATE-----\n")
    out.write(body)
    out.write("\n-----END CERTIFICATE-----\n")
    return out.toString().toByteArray()
}
