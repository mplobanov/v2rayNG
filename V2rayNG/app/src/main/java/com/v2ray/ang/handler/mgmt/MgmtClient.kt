package com.v2ray.ang.handler.mgmt

import android.content.Context
import com.google.gson.JsonArray
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.v2ray.ang.AppConfig
import com.v2ray.ang.handler.MmkvManager
import com.v2ray.ang.util.LogUtil
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import java.io.File
import java.io.RandomAccessFile

/**
 * Enrollment and the sync round-trip with the management server, shared by the VPN daemon's
 * agent and the idle WorkManager job. Syncs are serialised across processes with a file lock so
 * two processes never apply a configuration at the same time.
 */
object MgmtClient {
    class Outcome(
        val ok: Boolean,
        val status: String,
        val commands: List<JsonObject> = emptyList(),
        val error: String? = null,
        val configApplied: Boolean = false,
    )

    private val localLock = Mutex()

    private suspend fun <T> crossProcessLock(context: Context, block: suspend () -> T): T = localLock.withLock {
        val file = File(context.filesDir, "mgmt.lock")
        RandomAccessFile(file, "rw").use { raf ->
            raf.channel.lock().use { block() }
        }
    }

    private fun saveError(msg: String?) {
        MmkvManager.encodeSettings(MgmtSettings.KEY_LAST_ERROR, msg.orEmpty())
    }

    /** Enrolls if this install has no device token yet. */
    fun ensureEnrolled(context: Context): Boolean {
        if (MgmtSettings.deviceToken() != null) return true
        val token = MgmtSettings.enrollToken()
        if (token.isBlank()) {
            saveError("no enrollment token configured")
            return false
        }
        val body = JsonObject().apply {
            addProperty("enroll_token", token)
            addProperty("device_id", MgmtSettings.deviceId())
            addProperty("name", MgmtSettings.deviceName())
            add("device", MgmtTelemetry.deviceInfo(context))
        }
        val res = MgmtHttp.post("/v1/enroll", body, null)
        val devToken = res.body?.get("device_token")?.asString
        if (!res.ok || devToken.isNullOrBlank()) {
            saveError("enroll: ${res.error}")
            LogUtil.w(AppConfig.TAG, "Mgmt: enrollment failed: ${res.error}")
            return false
        }
        MmkvManager.encodeSettings(MgmtSettings.KEY_DEVICE_TOKEN, devToken)
        MmkvManager.encodeSettings(MgmtSettings.KEY_STATUS, res.body.get("status")?.asString.orEmpty())
        MmkvManager.removeSettings(MgmtSettings.KEY_STATE_HASHES)
        LogUtil.i(AppConfig.TAG, "Mgmt: enrolled, status=${res.body.get("status")?.asString}")
        return true
    }

    /**
     * One sync: uploads state plus queued events, measurements and command results; applies a
     * new configuration if the server sends one; returns the commands to execute.
     */
    suspend fun sync(
        context: Context,
        core: MgmtTelemetry.CoreFacts,
        events: List<JsonObject> = emptyList(),
        measurements: List<JsonObject> = emptyList(),
        results: List<JsonObject> = emptyList(),
        full: Boolean = false,
    ): Outcome {
        if (!MgmtSettings.isEnabled()) return Outcome(false, "disabled", error = "management disabled")
        return crossProcessLock(context) { syncLocked(context, core, events, measurements, results, full, retried = false) }
    }

    private fun syncLocked(
        context: Context,
        core: MgmtTelemetry.CoreFacts,
        events: List<JsonObject>,
        measurements: List<JsonObject>,
        results: List<JsonObject>,
        full: Boolean,
        retried: Boolean,
    ): Outcome {
        if (!ensureEnrolled(context)) return Outcome(false, "unenrolled", error = MgmtSettings.lastError())
        val snapshot = MgmtTelemetry.snapshot(context, core, full)
        val body = JsonObject().apply {
            addProperty("name", MgmtSettings.deviceName())
            add("device", MgmtTelemetry.deviceInfo(context))
            addProperty("applied_version", MgmtSettings.appliedVersion())
            MmkvManager.decodeSettingsString(MgmtSettings.KEY_PENDING_APPLY_RESULT)?.takeIf { it.isNotBlank() }?.let {
                add("apply_result", JsonParser.parseString(it))
            }
            add("state", snapshot.state)
            add("events", JsonArray().also { a -> events.forEach { a.add(it) } })
            add("measurements", JsonArray().also { a -> measurements.forEach { a.add(it) } })
            add("results", JsonArray().also { a -> results.forEach { a.add(it) } })
        }
        val res = MgmtHttp.post("/v1/sync", body, MgmtSettings.deviceToken(), compress = true)
        if (res.code == 401 && res.body?.get("reenroll")?.asBoolean == true && !retried) {
            MgmtSettings.forgetEnrollment()
            return syncLocked(context, core, events, measurements, results, full = true, retried = true)
        }
        if (!res.ok || res.body == null) {
            saveError("sync: ${res.error}")
            return Outcome(false, MgmtSettings.status(), error = res.error)
        }
        val resp = res.body
        MgmtTelemetry.acknowledge(snapshot)
        MmkvManager.removeSettings(MgmtSettings.KEY_PENDING_APPLY_RESULT)
        val status = resp.get("status")?.asString.orEmpty()
        MmkvManager.encodeSettings(MgmtSettings.KEY_STATUS, status)
        resp.getAsJsonObject("agent")?.let { MmkvManager.encodeSettings(MgmtSettings.KEY_AGENT_PARAMS, it.toString()) }
        MmkvManager.encodeSettings(MgmtSettings.KEY_LAST_SYNC, System.currentTimeMillis())
        saveError("")

        var applied = false
        val cfg = resp.get("config")?.takeIf { it.isJsonObject }?.asJsonObject
        if (cfg != null) {
            val version = cfg.get("version").asString
            val doc = cfg.getAsJsonObject("doc") ?: JsonObject()
            val result = MgmtConfigApplier.apply(context, version, doc)
            // A partly failed apply stays "not applied", so the next sync retries it (subscription
            // downloads, a server that appears later); every section is idempotent.
            if (result.errors.isEmpty()) {
                MmkvManager.encodeSettings(MgmtSettings.KEY_APPLIED_VERSION, version)
            }
            MmkvManager.encodeSettings(MgmtSettings.KEY_PENDING_APPLY_RESULT, result.toJson().toString())
            applied = true
        }
        val commands = resp.get("commands")?.takeIf { it.isJsonArray }?.asJsonArray
            ?.mapNotNull { if (it.isJsonObject) it.asJsonObject else null }.orEmpty()

        if (applied) {
            // Report the apply result (and the new settings) right away rather than next cycle.
            val follow = syncFollowUp(context, core)
            return Outcome(true, status, commands + follow, configApplied = true)
        }
        return Outcome(true, status, commands)
    }

    private fun syncFollowUp(context: Context, core: MgmtTelemetry.CoreFacts): List<JsonObject> {
        val snapshot = MgmtTelemetry.snapshot(context, core, full = false)
        val body = JsonObject().apply {
            addProperty("applied_version", MgmtSettings.appliedVersion())
            MmkvManager.decodeSettingsString(MgmtSettings.KEY_PENDING_APPLY_RESULT)?.takeIf { it.isNotBlank() }?.let {
                add("apply_result", JsonParser.parseString(it))
            }
            add("state", snapshot.state)
        }
        val res = MgmtHttp.post("/v1/sync", body, MgmtSettings.deviceToken(), compress = true)
        if (!res.ok || res.body == null) return emptyList()
        MgmtTelemetry.acknowledge(snapshot)
        MmkvManager.removeSettings(MgmtSettings.KEY_PENDING_APPLY_RESULT)
        res.body.getAsJsonObject("agent")?.let { MmkvManager.encodeSettings(MgmtSettings.KEY_AGENT_PARAMS, it.toString()) }
        // A config that keeps failing is retried at the next regular sync, not in a loop here.
        return res.body.get("commands")?.takeIf { it.isJsonArray }?.asJsonArray
            ?.mapNotNull { if (it.isJsonObject) it.asJsonObject else null }.orEmpty()
    }

    fun postResults(results: List<JsonObject>): Boolean {
        if (results.isEmpty()) return true
        val token = MgmtSettings.deviceToken() ?: return false
        val body = JsonObject().apply { add("results", JsonArray().also { a -> results.forEach { a.add(it) } }) }
        return MgmtHttp.post("/v1/results", body, token, compress = true).ok
    }

    /** Long poll: returns commands as soon as the server has some, or null on timeout/failure. */
    fun poll(waitSec: Int): MgmtHttp.HttpResult? {
        val token = MgmtSettings.deviceToken() ?: return null
        val v = java.net.URLEncoder.encode(MgmtSettings.appliedVersion(), "UTF-8")
        return MgmtHttp.get("/v1/poll?wait=$waitSec&v=$v", token, readTimeoutSec = waitSec + 30L)
    }
}
