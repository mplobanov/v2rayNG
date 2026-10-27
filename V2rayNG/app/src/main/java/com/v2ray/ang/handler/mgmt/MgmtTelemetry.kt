package com.v2ray.ang.handler.mgmt

import android.app.ActivityManager
import android.app.usage.UsageStatsManager
import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.wifi.WifiInfo
import android.net.wifi.WifiManager
import android.os.BatteryManager
import android.os.Build
import android.os.PowerManager
import android.telephony.TelephonyManager
import com.google.gson.Gson
import com.google.gson.JsonArray
import com.google.gson.JsonElement
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.v2ray.ang.AppConfig
import com.v2ray.ang.BuildConfig
import com.v2ray.ang.dto.entities.ProfileItem
import com.v2ray.ang.handler.MmkvManager
import com.v2ray.ang.handler.SettingsManager
import com.v2ray.ang.util.JsonUtil
import java.security.MessageDigest
import java.util.Locale
import java.util.TimeZone

/**
 * Collects what the management server needs to explain a connection problem: device, VPN,
 * access network (Wi-Fi / cellular details), power state, and the app's full configuration.
 *
 * Must not touch CoreServiceManager: outside the daemon process that would load the native core.
 * Callers pass the core facts they own through [CoreFacts].
 */
object MgmtTelemetry {
    class CoreFacts(
        val running: Boolean,
        val inDaemon: Boolean,
        val startedAtMs: Long = 0L,
        val traffic: Map<String, Long> = emptyMap(),
    )

    private val gson = Gson()

    fun deviceInfo(context: Context): JsonObject = JsonObject().apply {
        addProperty("manufacturer", Build.MANUFACTURER)
        addProperty("brand", Build.BRAND)
        addProperty("model", Build.MODEL)
        addProperty("device", Build.DEVICE)
        addProperty("android_release", Build.VERSION.RELEASE)
        addProperty("sdk", Build.VERSION.SDK_INT)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) addProperty("security_patch", Build.VERSION.SECURITY_PATCH)
        addProperty("abi", Build.SUPPORTED_ABIS.firstOrNull())
        addProperty("app_id", BuildConfig.APPLICATION_ID)
        addProperty("app_version", BuildConfig.VERSION_NAME)
        addProperty("app_code", BuildConfig.VERSION_CODE)
        addProperty("distribution", BuildConfig.DISTRIBUTION)
        addProperty("core_version", MmkvManager.decodeSettingsString(MgmtSettings.KEY_CORE_VERSION))
        addProperty("locale", Locale.getDefault().toLanguageTag())
        addProperty("timezone", TimeZone.getDefault().id)
        addProperty("device_name", MgmtSettings.deviceName())
    }

    fun profileSummary(guid: String, p: ProfileItem): JsonObject = JsonObject().apply {
        addProperty("guid", guid)
        addProperty("sub", p.subscriptionId)
        addProperty("remarks", p.remarks)
        addProperty("type", p.configType.name)
        addProperty("server", p.server)
        addProperty("port", p.serverPort)
        addProperty("network", p.network)
        addProperty("security", p.security)
        addProperty("sni", p.sni)
        addProperty("host", p.host)
        addProperty("path", p.path)
        addProperty("header", p.headerType)
        addProperty("flow", p.flow)
        addProperty("fp", p.fingerPrint)
        addProperty("alpn", p.alpn)
        addProperty("xhttp_mode", p.xhttpMode)
        MmkvManager.decodeServerAffiliationInfo(guid)?.let { addProperty("delay_ms", it.testDelayMillis) }
    }

    fun selectedServer(): JsonObject? {
        val guid = MmkvManager.getSelectServer() ?: return null
        val p = MmkvManager.decodeServerConfig(guid) ?: return null
        return profileSummary(guid, p).apply {
            MmkvManager.decodeSubscription(p.subscriptionId)?.let { addProperty("sub_remarks", it.remarks) }
        }
    }

    private fun vpnSection(core: CoreFacts): JsonObject = JsonObject().apply {
        addProperty("running", core.running)
        addProperty("reported_by", if (core.inDaemon) "daemon" else "idle-worker")
        addProperty("mode", when {
            SettingsManager.isRootMode() -> "root"
            SettingsManager.isVpnMode() -> "vpn"
            else -> "proxy"
        })
        addProperty("hev_tun", SettingsManager.isUsingHevTun())
        addProperty("loglevel", MmkvManager.decodeSettingsString(AppConfig.PREF_LOGLEVEL) ?: "warning")
        if (core.startedAtMs > 0 && core.running) {
            addProperty("core_uptime_s", (System.currentTimeMillis() - core.startedAtMs) / 1000)
        }
        add("selected", selectedServer())
        if (core.traffic.isNotEmpty()) {
            add("traffic_bytes", JsonObject().also { o -> core.traffic.forEach { (k, v) -> o.addProperty(k, v) } })
        }
    }

    private fun transports(caps: NetworkCapabilities): JsonArray = JsonArray().apply {
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) add("wifi")
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) add("cellular")
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) add("ethernet")
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_BLUETOOTH)) add("bluetooth")
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) add("vpn")
    }

    fun networkSummary(context: Context, cm: ConnectivityManager, network: Network?): JsonObject {
        val out = JsonObject()
        if (network == null) {
            out.addProperty("connected", false)
            return out
        }
        out.addProperty("connected", true)
        out.addProperty("id", network.toString())
        val caps = try { cm.getNetworkCapabilities(network) } catch (_: Exception) { null }
        if (caps != null) {
            out.add("transports", transports(caps))
            out.addProperty("validated", caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED))
            out.addProperty("captive_portal", caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_CAPTIVE_PORTAL))
            out.addProperty("metered", !caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED))
            out.addProperty("down_kbps", caps.linkDownstreamBandwidthKbps)
            out.addProperty("up_kbps", caps.linkUpstreamBandwidthKbps)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                out.addProperty("signal_strength", caps.signalStrength)
            }
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) out.add("wifi", wifiSection(context, caps))
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) out.add("cell", cellSection(context))
        }
        val lp: LinkProperties? = try { cm.getLinkProperties(network) } catch (_: Exception) { null }
        if (lp != null) {
            out.addProperty("iface", lp.interfaceName)
            out.add("dns", JsonArray().also { a -> lp.dnsServers.forEach { a.add(it.hostAddress) } })
            out.addProperty("has_ipv4", lp.linkAddresses.any { it.address is java.net.Inet4Address })
            out.addProperty("has_ipv6_global", lp.linkAddresses.any { it.address is java.net.Inet6Address && !it.address.isLinkLocalAddress })
            out.addProperty("domains", lp.domains)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
                out.addProperty("private_dns_active", lp.isPrivateDnsActive)
                out.addProperty("private_dns_server", lp.privateDnsServerName)
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) out.addProperty("mtu", lp.mtu)
        }
        return out
    }

    private fun wifiSection(context: Context, caps: NetworkCapabilities): JsonObject {
        val out = JsonObject()
        val info: WifiInfo? = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            caps.transportInfo as? WifiInfo ?: legacyWifiInfo(context)
        } else {
            legacyWifiInfo(context)
        }
        info ?: return out
        out.addProperty("ssid", info.ssid?.trim('"'))
        out.addProperty("rssi_dbm", info.rssi)
        out.addProperty("link_mbps", info.linkSpeed)
        out.addProperty("freq_mhz", info.frequency)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            out.addProperty("tx_mbps", info.txLinkSpeedMbps)
            out.addProperty("rx_mbps", info.rxLinkSpeedMbps)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) out.addProperty("standard", info.wifiStandard)
        return out
    }

    /**
     * WifiManager.getConnectionInfo() is deprecated since API 31 in favour of
     * NetworkCapabilities.getTransportInfo(), which only exists from API 29; it is used below API 29
     * and as a fallback when the capabilities carry no WifiInfo. Remove when minSdk reaches 29.
     */
    @Suppress("DEPRECATION")
    private fun legacyWifiInfo(context: Context): WifiInfo? = try {
        (context.applicationContext.getSystemService(Context.WIFI_SERVICE) as? WifiManager)?.connectionInfo
    } catch (_: Exception) {
        null
    }

    private fun cellSection(context: Context): JsonObject {
        val out = JsonObject()
        val tm = context.getSystemService(Context.TELEPHONY_SERVICE) as? TelephonyManager ?: return out
        try {
            out.addProperty("operator", tm.networkOperatorName)
            out.addProperty("mccmnc", tm.networkOperator)
            out.addProperty("sim_operator", tm.simOperatorName)
            out.addProperty("country", tm.networkCountryIso)
            out.addProperty("roaming", tm.isNetworkRoaming)
        } catch (_: Exception) {
        }
        try {
            out.addProperty("data_network_type", networkTypeName(tm.dataNetworkType))
        } catch (_: SecurityException) {
            out.addProperty("data_network_type", "needs READ_PHONE_STATE")
        } catch (_: Exception) {
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            try {
                val ss = tm.signalStrength
                if (ss != null) {
                    out.addProperty("signal_level", ss.level)
                    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                        ss.cellSignalStrengths.firstOrNull()?.let {
                            out.addProperty("signal_dbm", it.dbm)
                            out.addProperty("signal_type", it.javaClass.simpleName.removePrefix("CellSignalStrength"))
                        }
                    }
                }
            } catch (_: Exception) {
            }
        }
        return out
    }

    private fun networkTypeName(t: Int): String = when (t) {
        TelephonyManager.NETWORK_TYPE_LTE -> "LTE"
        20 -> "NR" // TelephonyManager.NETWORK_TYPE_NR (API 29)
        TelephonyManager.NETWORK_TYPE_HSPAP, TelephonyManager.NETWORK_TYPE_HSPA,
        TelephonyManager.NETWORK_TYPE_HSDPA, TelephonyManager.NETWORK_TYPE_HSUPA -> "HSPA"
        TelephonyManager.NETWORK_TYPE_UMTS -> "UMTS"
        TelephonyManager.NETWORK_TYPE_EDGE -> "EDGE"
        TelephonyManager.NETWORK_TYPE_GPRS -> "GPRS"
        TelephonyManager.NETWORK_TYPE_UNKNOWN -> "unknown"
        else -> "type$t"
    }

    private fun powerSection(context: Context): JsonObject {
        val out = JsonObject()
        val pm = context.getSystemService(Context.POWER_SERVICE) as? PowerManager
        if (pm != null) {
            out.addProperty("interactive", pm.isInteractive)
            out.addProperty("power_save", pm.isPowerSaveMode)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
                out.addProperty("device_idle", pm.isDeviceIdleMode)
                out.addProperty("ignoring_battery_optimizations", pm.isIgnoringBatteryOptimizations(context.packageName))
            }
        }
        val bm = context.getSystemService(Context.BATTERY_SERVICE) as? BatteryManager
        if (bm != null) {
            out.addProperty("battery_pct", bm.getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY))
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) out.addProperty("charging", bm.isCharging)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            (context.getSystemService(Context.ACTIVITY_SERVICE) as? ActivityManager)?.let {
                out.addProperty("background_restricted", it.isBackgroundRestricted)
            }
            (context.getSystemService(Context.USAGE_STATS_SERVICE) as? UsageStatsManager)?.let {
                out.addProperty("standby_bucket", it.appStandbyBucket)
            }
        }
        return out
    }

    private fun subscriptionsSection(): JsonArray = JsonArray().apply {
        MmkvManager.decodeSubscriptions().forEach { c ->
            add(JsonObject().apply {
                addProperty("guid", c.guid)
                addProperty("remarks", c.subscription.remarks)
                addProperty("url", c.subscription.url)
                addProperty("enabled", c.subscription.enabled)
                addProperty("auto_update", c.subscription.autoUpdate)
                addProperty("update_interval_min", c.subscription.updateInterval)
                addProperty("last_updated", c.subscription.lastUpdated)
                addProperty("servers", MmkvManager.decodeServerList(c.guid).size)
                addProperty("managed", c.guid.startsWith(MgmtConfigApplier.MANAGED_SUB_PREFIX))
                c.subscription.usageInfo?.let { add("usage", gson.toJsonTree(it)) }
            })
        }
    }

    private fun serversSection(): JsonArray = JsonArray().apply {
        var n = 0
        for (sub in MmkvManager.decodeSubscriptions()) {
            for (guid in MmkvManager.decodeServerList(sub.guid)) {
                if (n++ >= 500) return@apply
                MmkvManager.decodeServerConfig(guid)?.let { add(profileSummary(guid, it)) }
            }
        }
    }

    private fun routingSection(): JsonElement {
        val rules = MmkvManager.decodeRoutingRulesets() ?: return JsonArray()
        return JsonParser.parseString(JsonUtil.toJson(rules))
    }

    private fun hash(e: JsonElement): String =
        MessageDigest.getInstance("SHA-256").digest(e.toString().toByteArray()).joinToString("") { "%02x".format(it) }.take(16)

    class Snapshot(val state: JsonObject, val hashes: Map<String, String>)

    /**
     * Builds the state document. Heavy sections (settings, routing, subscriptions, servers) are
     * only included when they changed since the last acknowledged upload, or when [full].
     */
    fun snapshot(context: Context, core: CoreFacts, full: Boolean): Snapshot {
        val cm = context.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        val st = JsonObject()
        st.addProperty("ts", System.currentTimeMillis() / 1000.0)
        st.add("vpn", vpnSection(core))
        st.add("network", JsonObject().apply { add("default", networkSummary(context, cm, cm.activeNetwork)) })
        st.add("power", powerSection(context))
        st.add("mgmt", JsonObject().apply {
            addProperty("applied_version", MgmtSettings.appliedVersion())
            addProperty("live", MgmtSettings.isLive())
            addProperty("agent_alive", MgmtSettings.isAgentAlive())
        })

        val previous = JsonUtil.fromJsonSafe(
            MmkvManager.decodeSettingsString(MgmtSettings.KEY_STATE_HASHES).orEmpty(), Map::class.java
        ) ?: emptyMap<String, String>()
        val hashes = mutableMapOf<String, String>()
        val heavy = linkedMapOf<String, () -> JsonElement>(
            "settings" to { MgmtSettingsRegistry.dump() },
            "routing" to { routingSection() },
            "subscriptions" to { subscriptionsSection() },
            "servers" to { serversSection() },
        )
        heavy.forEach { (name, build) ->
            val value = build()
            val h = hash(value)
            hashes[name] = h
            if (full || previous[name] != h) st.add(name, value)
        }
        return Snapshot(st, hashes)
    }

    fun acknowledge(snapshot: Snapshot) {
        MmkvManager.encodeSettings(MgmtSettings.KEY_STATE_HASHES, JsonUtil.toJson(snapshot.hashes))
    }
}
