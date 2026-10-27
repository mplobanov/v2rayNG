package com.v2ray.ang.handler.mgmt

import com.v2ray.ang.AppConfig
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test

class MgmtLogShipperTest {

    @Test
    fun `parses a logcat epoch line`() {
        val line = MgmtLogShipper.parse("  1793100635.950  3684  3712 I GoLog   : from tcp:10.10.14.1:33156 accepted tcp:142.250.147.188:5228 [tun >> proxy]")

        assertNotNull(line)
        assertEquals(1793100635.950, line!!.ts, 1e-6)
        assertEquals(3684, line.pid)
        assertEquals("I", line.lvl)
        assertEquals("GoLog", line.tag)
        assertEquals("from tcp:10.10.14.1:33156 accepted tcp:142.250.147.188:5228 [tun >> proxy]", line.msg)
    }

    @Test
    fun `keeps colons inside the message`() {
        val line = MgmtLogShipper.parse("1793100635.000 10 11 E com.v2ray.ang.family: Caused by: java.net.ConnectException: refused")

        assertEquals("com.v2ray.ang.family", line!!.tag)
        assertEquals("Caused by: java.net.ConnectException: refused", line.msg)
    }

    @Test
    fun `ignores buffer headers and garbage`() {
        assertNull(MgmtLogShipper.parse("--------- beginning of main"))
        assertNull(MgmtLogShipper.parse(""))
    }

    @Test
    fun `expands the app placeholder in the filter spec`() {
        assertEquals(listOf("*:W", "GoLog:V", "${AppConfig.TAG}:V"), MgmtLogShipper.specArgs(" *:W  GoLog:V {app}:V "))
    }

    @Test
    fun `serialises lines for the logs endpoint`() {
        val json = MgmtLogShipper.toJson(listOf(MgmtLogShipper.Line(1.5, 2, "W", "GoLog", "m")), dropped = 3, source = "stream")

        assertEquals(1, json.getAsJsonArray("lines").size())
        assertEquals("GoLog", json.getAsJsonArray("lines")[0].asJsonObject.get("tag").asString)
        assertEquals(3, json.get("dropped").asInt)
    }

    @Test
    fun `managed subscription ids become safe guids`() {
        assertEquals("mgmt-cloud02", MgmtConfigApplier.managedSubGuid("cloud02"))
        assertEquals("mgmt-mama_phone", MgmtConfigApplier.managedSubGuid("mama/phone"))
    }
}
