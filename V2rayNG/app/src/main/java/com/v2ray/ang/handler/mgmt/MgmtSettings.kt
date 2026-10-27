package com.v2ray.ang.handler.mgmt

import android.os.Build
import com.google.gson.JsonElement
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.v2ray.ang.BuildConfig
import com.v2ray.ang.handler.MmkvManager
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull

/**
 * Persistent state of the remote-management client (logs & configs server, see mgmt/server).
 *
 * Kept in the multi-process settings store because the VPN daemon, the WorkManager process and
 * the UI process all read and write it. Every key starts with [PREFIX]; those keys are never
 * reported to, nor accepted from, the server's settings section.
 */
object MgmtSettings {
    const val PREFIX = "mgmt_"
    const val KEY_ENABLED = "mgmt_enabled"
    const val KEY_URL = "mgmt_url"
    const val KEY_PIN = "mgmt_pin"
    const val KEY_ENROLL_TOKEN = "mgmt_enroll_token"
    const val KEY_DEVICE_ID = "mgmt_device_id"
    const val KEY_DEVICE_TOKEN = "mgmt_device_token"
    const val KEY_DEVICE_NAME = "mgmt_device_name"
    const val KEY_STATUS = "mgmt_status"
    const val KEY_APPLIED_VERSION = "mgmt_applied_version"
    const val KEY_PENDING_APPLY_RESULT = "mgmt_pending_apply_result"
    const val KEY_LAST_SYNC = "mgmt_last_sync"
    const val KEY_LAST_ERROR = "mgmt_last_error"
    const val KEY_AGENT_PARAMS = "mgmt_agent_params"
    const val KEY_AGENT_ALIVE = "mgmt_agent_alive"
    const val KEY_MANAGED_SUBS = "mgmt_managed_subs"
    const val KEY_SERVERS_HASH = "mgmt_servers_hash"
    const val KEY_STATE_HASHES = "mgmt_state_hashes"
    const val KEY_LAST_LOG_TS = "mgmt_last_log_ts"
    const val KEY_CORE_VERSION = "mgmt_core_version"
    const val KEY_CORE_STARTED = "mgmt_core_started"
    const val KEY_WIFI_NAME = "mgmt_wifi_name"
    const val KEY_STARTED_COMMANDS = "mgmt_started_commands"
    const val KEY_URL_FAILURES = "mgmt_url_failures"
    const val KEY_URL_OK = "mgmt_url_ok"

    /** How long an address that could not be reached is tried only after the others. */
    private const val URL_FAILED_HOLD_MS = 10 * 60_000L

    fun isEnabled(): Boolean =
        MmkvManager.decodeSettingsBool(KEY_ENABLED, BuildConfig.MGMT_URL.isNotBlank()) && url().isNotBlank()

    fun url(): String = (MmkvManager.decodeSettingsString(KEY_URL).orEmpty().ifBlank { BuildConfig.MGMT_URL }).trimEnd('/')

    /**
     * Server addresses in order of preference. A URL set by hand on the management screen names
     * one specific server and is used alone; otherwise the server's own list (agent parameter
     * "mgmt_urls", from the server's device_urls) comes first and the address baked into the
     * build is the last resort. All of them share the pinned key.
     */
    fun urls(): List<String> =
        candidateUrls(MmkvManager.decodeSettingsString(KEY_URL), agentParams().get("mgmt_urls"), BuildConfig.MGMT_URL)

    /** [urls] with the ones that could not be reached lately moved behind the others. */
    fun urlsToTry(): List<String> =
        orderedByFailures(urls(), MmkvManager.decodeSettingsString(KEY_URL_FAILURES), System.currentTimeMillis())

    /** Records whether a request to [url] got a connection to the server. */
    fun recordUrl(url: String, reached: Boolean) {
        val raw = MmkvManager.decodeSettingsString(KEY_URL_FAILURES).orEmpty()
        val next = failuresWith(raw, url, reached, System.currentTimeMillis())
        if (next != raw) MmkvManager.encodeSettings(KEY_URL_FAILURES, next)
        if (reached && lastUrl() != url) MmkvManager.encodeSettings(KEY_URL_OK, url)
    }

    /** The address the last successful request went to. */
    fun lastUrl(): String = MmkvManager.decodeSettingsString(KEY_URL_OK).orEmpty()

    internal fun candidateUrls(manual: String?, pushed: JsonElement?, baked: String): List<String> {
        fun norm(u: String?, httpsOnly: Boolean): String? {
            val s = u?.trim()?.trimEnd('/') ?: return null
            val parsed = s.toHttpUrlOrNull() ?: return null
            return s.takeIf { !httpsOnly || parsed.isHttps }
        }
        norm(manual, httpsOnly = false)?.let { return listOf(it) }
        // Plain HTTP would send the device token in clear, so the server's list must be HTTPS.
        val fromServer = when {
            pushed == null || pushed.isJsonNull -> emptyList()
            pushed.isJsonArray -> pushed.asJsonArray.mapNotNull { e -> e.takeIf { it.isJsonPrimitive }?.asString }
            pushed.isJsonPrimitive -> pushed.asString.split(' ', ',', '\n')
            else -> emptyList()
        }.mapNotNull { norm(it, httpsOnly = true) }
        return (fromServer + listOfNotNull(norm(baked, httpsOnly = false))).distinct()
    }

    internal fun orderedByFailures(urls: List<String>, failuresRaw: String?, nowMs: Long): List<String> {
        val failed = parseFailures(failuresRaw).filterValues { nowMs - it < URL_FAILED_HOLD_MS }
        val (held, fresh) = urls.partition { it in failed }
        return fresh + held.sortedBy { failed[it] }
    }

    internal fun failuresWith(raw: String?, url: String, reached: Boolean, nowMs: Long): String {
        val m = parseFailures(raw).filterValues { nowMs - it < URL_FAILED_HOLD_MS }.toMutableMap()
        if (reached) m.remove(url) else m[url] = nowMs
        if (m.isEmpty()) return ""
        return JsonObject().apply { m.forEach { (k, v) -> addProperty(k, v) } }.toString()
    }

    private fun parseFailures(raw: String?): Map<String, Long> = try {
        if (raw.isNullOrBlank()) emptyMap()
        else JsonParser.parseString(raw).asJsonObject.entrySet().associate { it.key to it.value.asLong }
    } catch (_: Exception) {
        emptyMap()
    }

    fun pin(): String = MmkvManager.decodeSettingsString(KEY_PIN).orEmpty().ifBlank { BuildConfig.MGMT_PIN }

    fun enrollToken(): String =
        MmkvManager.decodeSettingsString(KEY_ENROLL_TOKEN).orEmpty().ifBlank { BuildConfig.MGMT_ENROLL_TOKEN }

    fun deviceId(): String {
        val existing = MmkvManager.decodeSettingsString(KEY_DEVICE_ID)
        if (!existing.isNullOrBlank()) return existing
        val id = java.util.UUID.randomUUID().toString()
        MmkvManager.encodeSettings(KEY_DEVICE_ID, id)
        return id
    }

    fun deviceToken(): String? = MmkvManager.decodeSettingsString(KEY_DEVICE_TOKEN)?.takeIf { it.isNotBlank() }

    fun deviceName(): String =
        MmkvManager.decodeSettingsString(KEY_DEVICE_NAME).orEmpty().ifBlank { "${Build.MANUFACTURER} ${Build.MODEL}" }

    fun status(): String = MmkvManager.decodeSettingsString(KEY_STATUS).orEmpty()

    fun appliedVersion(): String = MmkvManager.decodeSettingsString(KEY_APPLIED_VERSION).orEmpty()

    fun lastSync(): Long = MmkvManager.decodeSettingsLong(KEY_LAST_SYNC, 0L)

    fun lastError(): String = MmkvManager.decodeSettingsString(KEY_LAST_ERROR).orEmpty()

    /** Agent parameters from the last sync response; see the server's API.md "telemetry". */
    fun agentParams(): JsonObject {
        val raw = MmkvManager.decodeSettingsString(KEY_AGENT_PARAMS)
        return try {
            if (raw.isNullOrBlank()) JsonObject() else JsonParser.parseString(raw).asJsonObject
        } catch (_: Exception) {
            JsonObject()
        }
    }

    fun agentInt(name: String, default: Int): Int {
        val p = agentParams()
        return try {
            if (p.has(name) && !p.get(name).isJsonNull) p.get(name).asInt else default
        } catch (_: Exception) {
            default
        }
    }

    fun agentBool(name: String, default: Boolean): Boolean {
        val p = agentParams()
        return try {
            if (p.has(name) && !p.get(name).isJsonNull) p.get(name).asBoolean else default
        } catch (_: Exception) {
            default
        }
    }

    fun agentString(name: String, default: String): String {
        val p = agentParams()
        return try {
            if (p.has(name) && !p.get(name).isJsonNull) p.get(name).asString else default
        } catch (_: Exception) {
            default
        }
    }

    fun liveUntilMs(): Long = agentInt("live_until", 0).toLong() * 1000L

    fun isLive(): Boolean = liveUntilMs() > System.currentTimeMillis()

    /** True while the daemon-side agent runs; the idle worker then leaves syncing to it. */
    fun isAgentAlive(): Boolean {
        val alive = MmkvManager.decodeSettingsLong(KEY_AGENT_ALIVE, 0L)
        val window = maxOf(agentInt("sync_sec", 300), agentInt("poll_wait_sec", 240)) * 2_000L + 60_000L
        return System.currentTimeMillis() - alive < window
    }

    fun markAgentAlive(alive: Boolean) {
        MmkvManager.encodeSettings(KEY_AGENT_ALIVE, if (alive) System.currentTimeMillis() else 0L)
    }

    fun forgetEnrollment() {
        MmkvManager.removeSettings(KEY_DEVICE_TOKEN)
        MmkvManager.removeSettings(KEY_STATUS)
        MmkvManager.removeSettings(KEY_STATE_HASHES)
    }
}
