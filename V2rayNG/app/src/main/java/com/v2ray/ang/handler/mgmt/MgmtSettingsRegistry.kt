package com.v2ray.ang.handler.mgmt

import com.google.gson.JsonArray
import com.google.gson.JsonElement
import com.google.gson.JsonObject
import com.google.gson.JsonPrimitive
import com.v2ray.ang.AppConfig
import com.v2ray.ang.handler.MmkvManager

/**
 * Typed view of the settings store for the management server.
 *
 * MMKV does not record value types, so reading a key with the wrong decoder returns garbage. The
 * table below lists every user-facing setting with the type its readers use; keys the server
 * sends that are not listed get their type from the JSON value.
 */
object MgmtSettingsRegistry {
    enum class Type { BOOL, STRING, STRING_SET }

    val types: Map<String, Type> = buildMap {
        listOf(
            AppConfig.PREF_SNIFFING_ENABLED, AppConfig.PREF_ROUTE_ONLY_ENABLED, AppConfig.PREF_PER_APP_PROXY,
            AppConfig.PREF_BYPASS_APPS, AppConfig.PREF_LOCAL_DNS_ENABLED, AppConfig.PREF_FAKE_DNS_ENABLED,
            AppConfig.PREF_APPEND_HTTP_PROXY, AppConfig.PREF_MUX_ENABLED, AppConfig.PREF_FRAGMENT_ENABLED,
            AppConfig.PREF_SPEED_ENABLED, AppConfig.PREF_CONFIRM_REMOVE, AppConfig.PREF_DOUBLE_COLUMN_DISPLAY,
            AppConfig.PREF_GROUP_ALL_DISPLAY, AppConfig.PREF_DYNAMIC_COLOR, AppConfig.PREF_IPV6_ENABLED,
            AppConfig.PREF_PREFER_IPV6, AppConfig.PREF_PROXY_SHARING, AppConfig.PREF_ENABLE_LOCAL_PROXY,
            AppConfig.PREF_DYNAMIC_SOCKS_PORT, AppConfig.PREF_SOCKS_ENABLE_UDP, AppConfig.PREF_ROOT_MODE_ENABLE,
            AppConfig.PREF_ROOT_LAN_SHARING, AppConfig.PREF_IS_BOOTED, AppConfig.PREF_CHECK_UPDATE_PRE_RELEASE,
            AppConfig.PREF_USE_HEV_TUNNEL, AppConfig.PREF_UPDATE_SUBSCRIPTION,
            AppConfig.PREF_AUTO_TEST_AFTER_UPDATE_SUBSCRIPTION, AppConfig.PREF_AUTO_REMOVE_INVALID_AFTER_TEST,
            AppConfig.PREF_AUTO_SORT_AFTER_TEST,
        ).forEach { put(it, Type.BOOL) }
        listOf(
            AppConfig.PREF_LOCAL_DNS_PORT, AppConfig.PREF_VPN_DNS, AppConfig.PREF_VPN_BYPASS_LAN,
            AppConfig.PREF_VPN_INTERFACE_ADDRESS_CONFIG_INDEX, AppConfig.PREF_VPN_MTU,
            AppConfig.PREF_ROUTING_DOMAIN_STRATEGY, AppConfig.PREF_MUX_CONCURRENCY,
            AppConfig.PREF_MUX_XUDP_CONCURRENCY, AppConfig.PREF_MUX_XUDP_QUIC, AppConfig.PREF_FRAGMENT_PACKETS,
            AppConfig.PREF_FRAGMENT_LENGTH, AppConfig.PREF_FRAGMENT_INTERVAL, AppConfig.PREF_FRAGMENT_MAXSPLIT,
            AppConfig.PREF_OBSERVATORY_LEAST_PING_INTERVAL, AppConfig.PREF_OBSERVATORY_LEAST_LOAD_INTERVAL,
            AppConfig.PREF_OBSERVATORY_LEAST_LOAD_METHOD, AppConfig.PREF_OBSERVATORY_LEAST_LOAD_SAMPLING,
            AppConfig.PREF_OBSERVATORY_LEAST_LOAD_TIMEOUT, AppConfig.PREF_LANGUAGE, AppConfig.PREF_UI_MODE_NIGHT,
            AppConfig.PREF_SOCKS_PORT, AppConfig.PREF_SOCKS_USERNAME, AppConfig.PREF_SOCKS_PASSWORD,
            AppConfig.PREF_REMOTE_DNS, AppConfig.PREF_DOMESTIC_DNS, AppConfig.PREF_DNS_HOSTS,
            AppConfig.PREF_DELAY_TEST_URL, AppConfig.PREF_IP_API_URL, AppConfig.PREF_LOGLEVEL,
            AppConfig.PREF_OUTBOUND_DOMAIN_RESOLVE_METHOD, AppConfig.PREF_MODE, AppConfig.PREF_GEO_FILES_SOURCES,
            AppConfig.PREF_HEV_TUNNEL_LOGLEVEL, AppConfig.PREF_HEV_TUNNEL_RW_TIMEOUT,
            AppConfig.PREF_REAL_PING_CONCURRENCY,
        ).forEach { put(it, Type.STRING) }
        put(AppConfig.PREF_PER_APP_PROXY_SET, Type.STRING_SET)
    }

    /** Keys with their own section, internal bookkeeping, or this client's own state. */
    private val blocked = setOf(AppConfig.PREF_ROUTING_RULESET, AppConfig.PREF_APP_LOCALE_MIGRATED)

    /** Changes to these only affect the UI; everything else needs a core restart to take effect. */
    val uiOnlyKeys = setOf(
        AppConfig.PREF_CONFIRM_REMOVE, AppConfig.PREF_DOUBLE_COLUMN_DISPLAY, AppConfig.PREF_GROUP_ALL_DISPLAY,
        AppConfig.PREF_LANGUAGE, AppConfig.PREF_UI_MODE_NIGHT, AppConfig.PREF_DYNAMIC_COLOR, AppConfig.PREF_IS_BOOTED,
        AppConfig.PREF_CHECK_UPDATE_PRE_RELEASE, AppConfig.PREF_UPDATE_SUBSCRIPTION,
        AppConfig.PREF_AUTO_TEST_AFTER_UPDATE_SUBSCRIPTION, AppConfig.PREF_AUTO_REMOVE_INVALID_AFTER_TEST,
        AppConfig.PREF_AUTO_SORT_AFTER_TEST, AppConfig.PREF_IP_API_URL, AppConfig.PREF_REAL_PING_CONCURRENCY,
        AppConfig.PREF_GEO_FILES_SOURCES,
    )

    fun isWritable(key: String): Boolean = key !in blocked && !key.startsWith(MgmtSettings.PREFIX)

    /** Current values of all known settings that are set, plus the names of unknown keys. */
    fun dump(): JsonObject {
        val out = JsonObject()
        types.forEach { (key, type) ->
            if (!MmkvManager.containsSettingsKey(key)) return@forEach
            when (type) {
                Type.BOOL -> out.addProperty(key, MmkvManager.decodeSettingsBool(key, false))
                Type.STRING -> {
                    val v = MmkvManager.decodeSettingsString(key)
                    if (key == AppConfig.PREF_SOCKS_PASSWORD && !v.isNullOrEmpty()) out.addProperty(key, "***")
                    else out.addProperty(key, v)
                }
                Type.STRING_SET -> {
                    val arr = JsonArray()
                    MmkvManager.decodeSettingsStringSet(key)?.sorted()?.forEach { arr.add(it) }
                    out.add(key, arr)
                }
            }
        }
        val unknown = JsonArray()
        MmkvManager.allSettingsKeys()
            .filter { it !in types && it != AppConfig.PREF_ROUTING_RULESET && !it.startsWith(MgmtSettings.PREFIX) }
            .sorted()
            .forEach { unknown.add(it) }
        out.add("_other_keys", unknown)
        return out
    }

    private fun typeFor(key: String, value: JsonElement): Type? {
        types[key]?.let { return it }
        return when {
            value.isJsonArray -> Type.STRING_SET
            value.isJsonPrimitive && value.asJsonPrimitive.isBoolean -> Type.BOOL
            value.isJsonPrimitive -> Type.STRING
            else -> null
        }
    }

    private fun asBool(value: JsonElement): Boolean? {
        if (!value.isJsonPrimitive) return null
        val p = value.asJsonPrimitive
        return when {
            p.isBoolean -> p.asBoolean
            p.isString && p.asString.equals("true", true) -> true
            p.isString && p.asString.equals("false", true) -> false
            p.isNumber -> p.asInt != 0
            else -> null
        }
    }

    private fun asStringValue(value: JsonElement): String? {
        if (!value.isJsonPrimitive) return null
        val p: JsonPrimitive = value.asJsonPrimitive
        if (p.isNumber) {
            // 1500.0 from a lenient encoder must still become "1500".
            val d = p.asDouble
            return if (d == Math.floor(d) && !d.isInfinite()) d.toLong().toString() else p.asString
        }
        return p.asString
    }

    /**
     * Writes one setting. Returns true when the stored value changed.
     * @throws IllegalArgumentException when the value cannot be stored as the key's type.
     */
    fun applyOne(key: String, value: JsonElement?): Boolean {
        require(isWritable(key)) { "$key is not settable remotely" }
        if (value == null || value.isJsonNull) {
            if (!MmkvManager.containsSettingsKey(key)) return false
            MmkvManager.removeSettings(key)
            return true
        }
        val type = typeFor(key, value) ?: throw IllegalArgumentException("$key: unsupported value $value")
        val present = MmkvManager.containsSettingsKey(key)
        when (type) {
            Type.BOOL -> {
                val v = asBool(value) ?: throw IllegalArgumentException("$key expects a boolean")
                if (present && MmkvManager.decodeSettingsBool(key, !v) == v) return false
                MmkvManager.encodeSettings(key, v)
            }
            Type.STRING -> {
                val v = asStringValue(value) ?: throw IllegalArgumentException("$key expects a string")
                if (present && MmkvManager.decodeSettingsString(key) == v) return false
                MmkvManager.encodeSettings(key, v)
            }
            Type.STRING_SET -> {
                if (!value.isJsonArray) throw IllegalArgumentException("$key expects an array of strings")
                val v = value.asJsonArray.mapNotNull { if (it.isJsonPrimitive) it.asString else null }.toMutableSet()
                if (present && MmkvManager.decodeSettingsStringSet(key) == v) return false
                MmkvManager.encodeSettings(key, v)
            }
        }
        return true
    }
}
