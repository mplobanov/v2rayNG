package com.v2ray.ang.handler.mgmt

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test

class MgmtTelemetryTest {

    @Test
    fun `remembered Wi-Fi name is used for the same network only`() {
        val cache = "2629\tHomeWiFi_5G"

        assertEquals("HomeWiFi_5G", MgmtTelemetry.cachedWifiName(cache, "2629"))
        assertNull(MgmtTelemetry.cachedWifiName(cache, "2630"))
    }

    @Test
    fun `Wi-Fi name may contain tabs`() {
        assertEquals("a\tb", MgmtTelemetry.cachedWifiName("7\ta\tb", "7"))
    }

    @Test
    fun `missing or malformed cache yields no name`() {
        assertNull(MgmtTelemetry.cachedWifiName(null, "1"))
        assertNull(MgmtTelemetry.cachedWifiName("", "1"))
        assertNull(MgmtTelemetry.cachedWifiName("1", "1"))
        assertNull(MgmtTelemetry.cachedWifiName("1\t", "1"))
    }
}

class MgmtDnsTest {

    @Test
    fun `core DNS cache hits are dropped unless requested`() {
        val hit = MgmtLogShipper.Line(1.0, 1, "I", "GoLog", "DOH//dns.example.net:8443 cache HIT: a.com. -> [1.2.3.4]")
        val query = MgmtLogShipper.Line(1.0, 1, "I", "GoLog", "DOH//dns.example.net:8443 got answer: a.com. -> [1.2.3.4] 40ms")
        val app = MgmtLogShipper.Line(1.0, 1, "I", "v2rayNG", "cache HIT: is fine here")

        assertEquals(false, MgmtLogShipper.keep(hit, dnsCacheLines = false))
        assertEquals(true, MgmtLogShipper.keep(hit, dnsCacheLines = true))
        assertEquals(true, MgmtLogShipper.keep(query, dnsCacheLines = false))
        assertEquals(true, MgmtLogShipper.keep(app, dnsCacheLines = false))
    }

    @Test
    fun `answer count comes from the DNS header`() {
        val header = ByteArray(12).also { it[6] = 0x01; it[7] = 0x02 }

        assertEquals(258, MgmtProbes.dnsAnswerCount(header))
        assertEquals(0, MgmtProbes.dnsAnswerCount(ByteArray(5)))
        assertEquals(0, MgmtProbes.dnsAnswerCount(null))
    }
}
