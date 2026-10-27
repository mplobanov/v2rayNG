package com.v2ray.ang.handler.mgmt

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

class MgmtProbesTest {

    private fun awaitStuck(count: Int) {
        val until = System.currentTimeMillis() + 5_000
        while (MgmtProbes.stuckDelayTests() != count && System.currentTimeMillis() < until) Thread.sleep(10)
        assertEquals(count, MgmtProbes.stuckDelayTests())
    }

    @Test
    fun `a delay test that returns in time gives its value`() {
        assertEquals(42L, MgmtProbes.withDeadline(1_000) { 42L })
    }

    @Test
    fun `a delay test's own failure is rethrown`() {
        val e = assertThrows(IllegalStateException::class.java) {
            MgmtProbes.withDeadline<Long>(1_000) { throw IllegalStateException("core start failed") }
        }
        assertEquals("core start failed", e.message)
    }

    @Test
    fun `a delay test that never returns is abandoned at the deadline`() {
        val base = MgmtProbes.stuckDelayTests()
        val release = CountDownLatch(1)
        val start = System.nanoTime()

        val e = assertThrows(MgmtProbes.DeadlineException::class.java) {
            MgmtProbes.withDeadline(200) { release.await(); 1L }
        }

        val ms = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - start)
        assertEquals("timeout", e.message)
        assertTrue(ms in 200..2_000, "returned after $ms ms")
        assertEquals(base + 1, MgmtProbes.stuckDelayTests())
        // The native call returning later frees its slot.
        release.countDown()
        awaitStuck(base)
    }

    @Test
    fun `new delay tests are skipped while too many are stuck`() {
        val base = MgmtProbes.stuckDelayTests()
        val release = CountDownLatch(1)
        var stuck = 0
        while (true) {
            try {
                MgmtProbes.withDeadline(50) { release.await(); 1L }
            } catch (e: MgmtProbes.DeadlineException) {
                if (e.message != "timeout") break
                stuck++
            }
            assertTrue(stuck <= 10, "never skipped")
        }
        assertEquals(4, base + stuck)

        var ran = false
        val e = assertThrows(MgmtProbes.DeadlineException::class.java) {
            MgmtProbes.withDeadline(1_000) { ran = true; 1L }
        }
        assertTrue(e.message!!.startsWith("skipped: 4 "), e.message)
        assertFalse(ran)

        release.countDown()
        awaitStuck(base)
        assertEquals(7L, MgmtProbes.withDeadline(1_000) { 7L })
    }

    @Test
    fun `the hard limit is twice the probe timeout but never below the native timeout`() {
        assertEquals(16_000L, MgmtProbes.delayDeadlineMs(8_000))
        assertEquals(15_000L, MgmtProbes.delayDeadlineMs(3_000))
        assertEquals(40_000L, MgmtProbes.delayDeadlineMs(20_000))
    }
}
