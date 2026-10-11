package app.lararium

import app.lararium.data.EndpointPolicy
import app.lararium.data.PairingLink
import org.junit.Assert.*
import org.junit.Test

/**
 * The cleartext policy is a trust boundary: tokens must never cross the
 * public internet unencrypted. Pin the exact accepted set here — widening
 * it must fail this test on purpose.
 */
class EndpointPolicyTest {

    private fun ok(url: String) = assertNull(EndpointPolicy.rejectReason(url))
    private fun no(url: String) = assertNotNull(EndpointPolicy.rejectReason(url))

    @Test fun httpsAnywhereAllowed() {
        ok("https://hearth.example.com:7717")
        ok("https://100.111.222.33:7717")
    }

    @Test fun httpOnlyOnPrivateNetworks() {
        ok("http://192.168.99.99:7717")
        ok("http://10.0.3.111:7717")
        ok("http://172.16.0.5:7717")
        ok("http://100.123.45.67:7717")  // tailnet
        ok("http://localhost:7717")
        no("http://9.255.255.255:7717")  // just outside RFC1918
        no("http://172.32.0.1:7717")     // 172.32 is NOT private
        no("http://100.63.0.1:7717")     // below CGNAT block
        no("http://example.com:7717")
    }

    @Test fun rejectsGarbage() {
        no("not a url")
        no("ftp://host:21")
        no("http://")
    }

    @Test fun pairLinkParses() {
        val (ep, _) = PairingLink.parse(
            "lararium://pair?host=http%3A%2F%2F192.168.99.99%3A7717&token=lar1_abc123&name=home"
        )!!
        assertEquals("http://192.168.99.99:7717", ep.baseUrl)
        assertEquals("lar1_abc123", ep.token)
        assertEquals("home", ep.name)
    }

    @Test fun pairLinkRejectsPublicCleartext() {
        assertNull(PairingLink.parse(
            "lararium://pair?host=http%3A%2F%2Fevil.example.com%3A7717&token=lar1_x"
        ))
    }

    @Test fun pairLinkRejectsForeignScheme() {
        assertNull(PairingLink.parse("https://pair?host=x&token=***"))
        assertNull(PairingLink.parse("garbage"))
    }
}
