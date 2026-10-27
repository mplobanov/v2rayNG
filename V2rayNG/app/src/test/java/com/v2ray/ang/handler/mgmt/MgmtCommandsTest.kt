package com.v2ray.ang.handler.mgmt

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test

class MgmtCommandsTest {

    private val day = 86400L

    @Test
    fun `a new command id is recorded`() {
        assertEquals("13:1000", MgmtCommands.startedWith(null, 13, 1000))
        assertEquals("13:1000,15:1010", MgmtCommands.startedWith("13:1000", 15, 1010))
    }

    @Test
    fun `a repeated command id is refused`() {
        assertNull(MgmtCommands.startedWith("13:1000,20:1005", 20, 1100))
        assertNull(MgmtCommands.startedWith("13:1000,20:1005", 13, 1100))
    }

    @Test
    fun `ids older than a week are forgotten`() {
        val now = 10 * day
        assertEquals("2:${now - day},3:$now", MgmtCommands.startedWith("1:${now - 7 * day},2:${now - day}", 3, now))
    }

    @Test
    fun `the record keeps the newest 200 ids`() {
        val raw = (1L..200L).joinToString(",") { "$it:1000" }

        val out = MgmtCommands.startedWith(raw, 201, 1001)!!.split(',')

        assertEquals(200, out.size)
        assertEquals("2:1000", out.first())
        assertEquals("201:1001", out.last())
    }

    @Test
    fun `malformed entries are dropped`() {
        assertEquals("5:1000,7:1000", MgmtCommands.startedWith("x,5:1000,:3,6:,6", 7, 1000))
    }
}
