package com.v2ray.ang.handler.mgmt

import com.google.gson.JsonArray
import com.google.gson.JsonPrimitive
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class MgmtUrlsTest {

    private val baked = "https://192.0.2.1:9443"
    private val name = "https://mgmt.example.net:9443"
    private val other = "https://other.example.net:9443"
    private val min = 60_000L

    private fun list(vararg s: String) = JsonArray().apply { s.forEach { add(it) } }

    @Test
    fun `the baked address alone without a server list`() {
        assertEquals(listOf(baked), MgmtSettings.candidateUrls(null, null, baked))
        assertEquals(listOf(baked), MgmtSettings.candidateUrls("  ", null, baked))
    }

    @Test
    fun `the server list comes first and the baked address last`() {
        assertEquals(listOf(name, baked), MgmtSettings.candidateUrls(null, list("$name/", baked), baked))
        assertEquals(listOf(name, other, baked), MgmtSettings.candidateUrls(null, list(name, other), baked))
    }

    @Test
    fun `a space separated string is a list too`() {
        assertEquals(listOf(name, other, baked), MgmtSettings.candidateUrls(null, JsonPrimitive("$name $other"), baked))
    }

    @Test
    fun `the server list must be https`() {
        val got = MgmtSettings.candidateUrls(null, list("http://mgmt.example.net:9080", "not a url", name), baked)
        assertEquals(listOf(name, baked), got)
    }

    @Test
    fun `a hand-set address is used alone`() {
        assertEquals(listOf("http://10.0.2.2:9080"), MgmtSettings.candidateUrls("http://10.0.2.2:9080/", list(name), baked))
    }

    @Test
    fun `addresses that failed lately go last, oldest failure first`() {
        val now = 100 * min
        var raw = MgmtSettings.failuresWith(null, name, reached = false, now - 2 * min)
        raw = MgmtSettings.failuresWith(raw, other, reached = false, now - 3 * min)
        assertEquals(listOf(baked, other, name), MgmtSettings.orderedByFailures(listOf(name, other, baked), raw, now))
    }

    @Test
    fun `a failure is forgotten after ten minutes or on success`() {
        val now = 100 * min
        val raw = MgmtSettings.failuresWith(null, name, reached = false, now - 11 * min)
        assertEquals(listOf(name, baked), MgmtSettings.orderedByFailures(listOf(name, baked), raw, now))
        assertEquals("", MgmtSettings.failuresWith(raw, baked, reached = true, now))

        val fresh = MgmtSettings.failuresWith(null, name, reached = false, now)
        assertEquals("", MgmtSettings.failuresWith(fresh, name, reached = true, now + min))
    }

    @Test
    fun `a broken record is ignored`() {
        assertEquals(listOf(name, baked), MgmtSettings.orderedByFailures(listOf(name, baked), "{oops", 0))
    }
}
