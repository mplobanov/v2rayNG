package com.v2ray.ang.handler.mgmt

import com.google.gson.JsonArray
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.v2ray.ang.handler.MmkvManager

/**
 * Persistent outbox of device events (VPN start/stop/failure, network changes, ...).
 *
 * Stored in the multi-process settings store so an event recorded by the daemon right before
 * it dies, or while the network is down, is still delivered by whichever process syncs next.
 */
object MgmtEvents {
    private const val KEY = "mgmt_pending_events"
    private const val MAX = 300

    private fun load(): JsonArray = try {
        val raw = MmkvManager.decodeSettingsString(KEY)
        if (raw.isNullOrBlank()) JsonArray() else JsonParser.parseString(raw).asJsonArray
    } catch (_: Exception) {
        JsonArray()
    }

    @Synchronized
    fun add(type: String, data: JsonObject = JsonObject()) {
        if (!MgmtSettings.isEnabled()) return
        val arr = load()
        arr.add(JsonObject().apply {
            addProperty("ts", System.currentTimeMillis() / 1000.0)
            addProperty("type", type)
            add("data", data)
        })
        while (arr.size() > MAX) arr.remove(0)
        MmkvManager.encodeSettings(KEY, arr.toString())
    }

    @Synchronized
    fun drain(): List<JsonObject> {
        val arr = load()
        if (arr.size() == 0) return emptyList()
        MmkvManager.removeSettings(KEY)
        return arr.mapNotNull { if (it.isJsonObject) it.asJsonObject else null }
    }

    @Synchronized
    fun requeue(events: List<JsonObject>) {
        if (events.isEmpty()) return
        val arr = JsonArray()
        events.forEach { arr.add(it) }
        load().forEach { arr.add(it) }
        while (arr.size() > MAX) arr.remove(0)
        MmkvManager.encodeSettings(KEY, arr.toString())
    }
}
