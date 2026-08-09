package cloud.spawnery.agent

import org.bouncycastle.asn1.x500.X500Name
import org.bouncycastle.asn1.x509.BasicConstraints
import org.bouncycastle.asn1.x509.Extension
import org.bouncycastle.asn1.x509.GeneralName
import org.bouncycastle.asn1.x509.GeneralNames
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
    ).addExtension(Extension.basicConstraints, true, BasicConstraints(true))
    val signer = JcaContentSignerBuilder("SHA256withRSA").build(keyPair.private)
    val certificate = JcaX509CertificateConverter().getCertificate(builder.build(signer))
    return TestCa(certificate, keyPair)
}

/** A serving certificate for [dnsName], signed by [ca]. */
data class TestServingCertificate(val certificate: X509Certificate, val keyPair: KeyPair)

fun newServingCertificate(ca: TestCa, dnsName: String): TestServingCertificate {
    val keyPair = KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair()
    val now = System.currentTimeMillis()
    val builder = JcaX509v3CertificateBuilder(
        X500Name(ca.certificate.subjectX500Principal.name),
        BigInteger.valueOf(now + 1),
        Date(now - 60_000),
        Date(now + 3_600_000),
        X500Name("CN=$dnsName"),
        keyPair.public,
    ).addExtension(
        // The name, not the subject: gRPC's hostname verifier reads the SAN
        // and ignores the common name, exactly as a browser would.
        Extension.subjectAlternativeName,
        false,
        GeneralNames(GeneralName(GeneralName.dNSName, dnsName)),
    )
    val signer = JcaContentSignerBuilder("SHA256withRSA").build(ca.keyPair.private)
    val certificate = JcaX509CertificateConverter().getCertificate(builder.build(signer))
    return TestServingCertificate(certificate, keyPair)
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
