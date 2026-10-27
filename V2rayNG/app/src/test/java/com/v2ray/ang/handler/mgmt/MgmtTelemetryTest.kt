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
