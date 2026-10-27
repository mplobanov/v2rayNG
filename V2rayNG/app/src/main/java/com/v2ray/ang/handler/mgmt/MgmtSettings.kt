package com.v2ray.ang.handler.mgmt

import android.os.Build
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.v2ray.ang.BuildConfig
import com.v2ray.ang.handler.MmkvManager

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

    fun isEnabled(): Boolean =
        MmkvManager.decodeSettingsBool(KEY_ENABLED, BuildConfig.MGMT_URL.isNotBlank()) && url().isNotBlank()

    fun url(): String = (MmkvManager.decodeSettingsString(KEY_URL).orEmpty().ifBlank { BuildConfig.MGMT_URL }).trimEnd('/')

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
