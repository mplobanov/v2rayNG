package com.v2ray.ang.handler.mgmt

import android.content.Context
import com.google.gson.JsonArray
import com.google.gson.JsonElement
import com.google.gson.JsonObject
import com.v2ray.ang.AppConfig
import com.v2ray.ang.core.CoreConfigManager
import com.v2ray.ang.core.CoreNativeManager
import com.v2ray.ang.dto.entities.ProfileItem
import com.v2ray.ang.enums.EConfigType
import com.v2ray.ang.handler.MmkvManager
import com.v2ray.ang.handler.SettingsManager
import okhttp3.Call
import okhttp3.EventListener
import okhttp3.OkHttpClient
import okhttp3.Request
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.Proxy
import java.net.Socket
import java.security.SecureRandom
import java.security.cert.X509Certificate
import java.util.concurrent.TimeUnit
import javax.net.ssl.SNIHostName
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509TrustManager

/**
 * Network measurements run on the device. "Direct" probes leave through the physical network
 * (this app is excluded from its own VPN), "proxy" probes go through the core's local inbound,
 * so comparing the two separates an ISP/DPI problem from a proxy/server problem.
 */
object MgmtProbes {
    class Measurement(val kind: String, val target: String, val ok: Boolean, val ms: Double, val data: JsonObject = JsonObject()) {
        val ts = System.currentTimeMillis() / 1000.0
        fun toJson(): JsonObject = JsonObject().apply {
            addProperty("ts", ts)
            addProperty("kind", kind)
            addProperty("target", target)
            addProperty("ok", ok)
            addProperty("ms", ms)
            add("data", data)
        }
    }

    private fun elapsed(start: Long) = (System.nanoTime() - start) / 1e6

    private fun err(e: Throwable): JsonObject = JsonObject().apply {
        addProperty("error", "${e.javaClass.simpleName}: ${e.message}")
    }

    private fun isUdpOnly(p: ProfileItem) =
        p.configType == EConfigType.HYSTERIA2 || p.configType == EConfigType.WIREGUARD ||
            p.configType.name == "TUIC" || p.configType.name == "HYSTERIA"

    fun tcpConnect(label: String, host: String, port: Int, timeoutMs: Int): Measurement {
        val start = System.nanoTime()
        return try {
            val addr = InetAddress.getByName(host)
            val dnsMs = elapsed(start)
            Socket().use { s ->
                val t = System.nanoTime()
                s.connect(InetSocketAddress(addr, port), timeoutMs)
                Measurement("tcp_connect", "$label $host:$port", true, elapsed(t),
                    JsonObject().apply { addProperty("ip", addr.hostAddress); addProperty("dns_ms", dnsMs) })
            }
        } catch (e: Exception) {
            Measurement("tcp_connect", "$label $host:$port", false, -1.0, err(e).apply { addProperty("after_ms", elapsed(start)) })
        }
    }

    private val trustAll = object : X509TrustManager {
        override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
        override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }

    /**
     * Direct TLS handshake with the profile's SNI. A REALITY server forwards unauthenticated
     * handshakes to its camouflage site, so success means the path to host:port carries a full
     * ClientHello; DPI that drops the ClientHello shows up as a timeout. Certificates are not
     * validated: only reachability and timing are measured.
     */
    fun tlsHandshake(label: String, host: String, port: Int, sni: String, timeoutMs: Int): Measurement {
        val target = "$label $host:$port sni=$sni"
        val start = System.nanoTime()
        return try {
            val addr = InetAddress.getByName(host)
            val ctx = SSLContext.getInstance("TLS").apply { init(null, arrayOf(trustAll), SecureRandom()) }
            Socket().use { raw ->
                raw.connect(InetSocketAddress(addr, port), timeoutMs)
                val connectMs = elapsed(start)
                raw.soTimeout = timeoutMs
                val ssl = ctx.socketFactory.createSocket(raw, sni, port, true) as SSLSocket
                ssl.sslParameters = ssl.sslParameters.apply { serverNames = listOf(SNIHostName(sni)) }
                val t = System.nanoTime()
                ssl.startHandshake()
                val hs = elapsed(t)
                val data = JsonObject().apply {
                    addProperty("connect_ms", connectMs)
                    addProperty("protocol", ssl.session.protocol)
                    addProperty("ip", addr.hostAddress)
                    (ssl.session.peerCertificates.firstOrNull() as? X509Certificate)?.let {
                        addProperty("cert_subject", it.subjectX500Principal.name.take(120))
                    }
                }
                ssl.close()
                Measurement("tls_handshake", target, true, hs, data)
            }
        } catch (e: Exception) {
            Measurement("tls_handshake", target, false, -1.0, err(e).apply { addProperty("after_ms", elapsed(start)) })
        }
    }

    private class Timings : EventListener() {
        var start = 0L
        var dns = -1.0
        var connect = -1.0
        var tls = -1.0
        private var dnsStart = 0L
        private var connStart = 0L
        private var tlsStart = 0L
        override fun callStart(call: Call) { start = System.nanoTime() }
        override fun dnsStart(call: Call, domainName: String) { dnsStart = System.nanoTime() }
        override fun dnsEnd(call: Call, domainName: String, inetAddressList: List<InetAddress>) { dns = elapsed(dnsStart) }
        override fun connectStart(call: Call, inetSocketAddress: InetSocketAddress, proxy: Proxy) { connStart = System.nanoTime() }
        override fun secureConnectStart(call: Call) { tlsStart = System.nanoTime(); connect = elapsed(connStart) }
        override fun secureConnectEnd(call: Call, handshake: okhttp3.Handshake?) { tls = elapsed(tlsStart) }
    }

    private fun httpClient(viaProxy: Boolean, timeoutMs: Int, timings: Timings): OkHttpClient {
        val b = OkHttpClient.Builder()
            .connectTimeout(timeoutMs.toLong(), TimeUnit.MILLISECONDS)
            .readTimeout(timeoutMs.toLong(), TimeUnit.MILLISECONDS)
            .callTimeout(timeoutMs.toLong() * 3, TimeUnit.MILLISECONDS)
            .followRedirects(false)
            .eventListener(timings)
        if (viaProxy) {
            b.proxy(Proxy(Proxy.Type.HTTP, InetSocketAddress(AppConfig.LOOPBACK, SettingsManager.getHttpPort())))
            val user = SettingsManager.getSocksUsername()
            val pass = SettingsManager.getSocksPassword()
            if (user != null && pass != null) {
                b.proxyAuthenticator { _, resp ->
                    resp.request.newBuilder().header("Proxy-Authorization", okhttp3.Credentials.basic(user, pass)).build()
                }
            }
        } else {
            b.proxy(Proxy.NO_PROXY)
        }
        return b.build()
    }

    fun http(url: String, viaProxy: Boolean, timeoutMs: Int, maxBytes: Long = 1 shl 20, kind: String? = null): Measurement {
        val k = kind ?: if (viaProxy) "http_proxy" else "http_direct"
        val timings = Timings()
        val start = System.nanoTime()
        return try {
            httpClient(viaProxy, timeoutMs, timings).newCall(Request.Builder().url(url).header("User-Agent", "Mozilla/5.0").build()).execute().use { resp ->
                val ttfb = elapsed(start)
                var bytes = 0L
                resp.body?.byteStream()?.use { s ->
                    val buf = ByteArray(16 * 1024)
                    while (bytes < maxBytes) {
                        val n = s.read(buf)
                        if (n < 0) break
                        bytes += n
                    }
                }
                val total = elapsed(start)
                val data = JsonObject().apply {
                    addProperty("status", resp.code)
                    addProperty("ttfb_ms", ttfb)
                    addProperty("bytes", bytes)
                    addProperty("dns_ms", timings.dns)
                    addProperty("connect_ms", timings.connect)
                    addProperty("tls_ms", timings.tls)
                    if (k == "download" && total > 0) addProperty("mbps", bytes * 8 / 1e6 / (total / 1000))
                }
                Measurement(k, url, resp.code in 200..399, total, data)
            }
        } catch (e: Exception) {
            Measurement(k, url, false, -1.0, err(e).apply {
                addProperty("after_ms", elapsed(start))
                addProperty("dns_ms", timings.dns)
                addProperty("connect_ms", timings.connect)
                addProperty("tls_ms", timings.tls)
            })
        }
    }

    fun dns(host: String): Measurement {
        val start = System.nanoTime()
        return try {
            val addrs = InetAddress.getAllByName(host)
            Measurement("dns_system", host, true, elapsed(start), JsonObject().apply {
                add("addrs", JsonArray().also { a -> addrs.take(8).forEach { a.add(it.hostAddress) } })
            })
        } catch (e: Exception) {
            Measurement("dns_system", host, false, -1.0, err(e))
        }
    }

    fun realPing(context: Context, guid: String, remarks: String): Measurement {
        val start = System.nanoTime()
        return try {
            CoreNativeManager.initCoreEnv(context)
            val cfg = CoreConfigManager.getV2rayConfig4Speedtest(context, guid)
            if (!cfg.status) return Measurement("real_ping", remarks, false, -1.0, JsonObject().apply { addProperty("error", cfg.errorMessage) })
            val ms = CoreNativeManager.measureOutboundDelay(cfg.content, SettingsManager.getDelayTestUrl())
            Measurement("real_ping", remarks, ms >= 0, ms.toDouble(), JsonObject().apply { addProperty("wall_ms", elapsed(start)) })
        } catch (e: Exception) {
            Measurement("real_ping", remarks, false, -1.0, err(e))
        }
    }

    fun coreDelay(remarks: String, measure: (String) -> Long): Measurement {
        val url = SettingsManager.getDelayTestUrl()
        val start = System.nanoTime()
        return try {
            val ms = measure(url)
            Measurement("core_delay", remarks, ms >= 0, ms.toDouble(), JsonObject().apply { addProperty("url", url) })
        } catch (e: Exception) {
            Measurement("core_delay", remarks, false, -1.0, err(e).apply { addProperty("after_ms", elapsed(start)) })
        }
    }

    /** Resolves the "servers" probe argument to (guid, profile) pairs. */
    fun serversFor(spec: JsonElement?): List<Pair<String, ProfileItem>> {
        val selected = MmkvManager.getSelectServer()
        val guids: List<String> = when {
            spec == null || spec.isJsonNull || (spec.isJsonPrimitive && spec.asString == "selected") -> listOfNotNull(selected)
            spec.isJsonPrimitive && spec.asString == "group" -> {
                val sub = selected?.let { MmkvManager.decodeServerConfig(it)?.subscriptionId }
                if (sub == null) listOfNotNull(selected) else MmkvManager.decodeServerList(sub)
            }
            spec.isJsonPrimitive && spec.asString == "all" -> MmkvManager.decodeAllServerList()
            spec.isJsonArray -> spec.asJsonArray.mapNotNull { e -> MgmtConfigApplier.findServer(e.asString, null) }
            else -> listOfNotNull(selected)
        }
        return guids.distinct().take(60).mapNotNull { g -> MmkvManager.decodeServerConfig(g)?.let { g to it } }
    }

    private fun JsonObject.flag(name: String, default: Boolean) =
        if (has(name) && get(name).isJsonPrimitive) get(name).asBoolean else default

    private fun JsonObject.strings(name: String, default: List<String>): List<String> =
        if (has(name) && get(name).isJsonArray) getAsJsonArray(name).map { it.asString } else default

    fun directServerProbes(p: ProfileItem, timeoutMs: Int, tcp: Boolean, tls: Boolean): List<Measurement> {
        val host = p.server ?: return emptyList()
        val port = p.serverPort?.toIntOrNull() ?: return emptyList()
        if (isUdpOnly(p)) return emptyList()
        val out = mutableListOf<Measurement>()
        if (tcp) out += tcpConnect(p.remarks, host, port, timeoutMs)
        val sec = p.security.orEmpty()
        if (tls && (sec == AppConfig.TLS || sec == AppConfig.REALITY)) {
            val sni = p.sni?.takeIf { it.isNotBlank() } ?: p.host?.takeIf { it.isNotBlank() } ?: host
            out += tlsHandshake(p.remarks, host, port, sni, timeoutMs)
        }
        return out
    }

    /** The on-demand "probe" command. */
    fun run(context: Context, args: JsonObject, coreRunning: Boolean, coreDelayFn: ((String) -> Long)?): List<Measurement> {
        val timeout = if (args.has("timeout_ms")) args.get("timeout_ms").asInt else 8000
        val out = mutableListOf<Measurement>()
        val servers = serversFor(args.get("servers"))
        val selected = MmkvManager.getSelectServer()
        servers.forEach { (guid, p) ->
            out += directServerProbes(p, timeout, args.flag("tcp", true), args.flag("tls", true))
            if (args.flag("real_ping", true)) out += realPing(context, guid, p.remarks)
            if (guid == selected && coreRunning && coreDelayFn != null && args.flag("core_delay", true)) {
                out += coreDelay(p.remarks, coreDelayFn)
            }
        }
        if (coreRunning) {
            args.strings("urls_proxy", listOf("https://www.gstatic.com/generate_204", "https://www.youtube.com/generate_204"))
                .forEach { out += http(it, true, timeout) }
        }
        args.strings("urls_direct", listOf("https://ya.ru/")).forEach { out += http(it, false, timeout) }
        args.strings("dns", emptyList()).forEach { out += dns(it) }
        if (args.has("download_url")) {
            val via = !args.has("download_via") || args.get("download_via").asString != "direct"
            out += http(args.get("download_url").asString, via && coreRunning, timeout * 4, 64L shl 20, kind = "download")
        }
        return out
    }

    /** The periodic background health check of the daemon agent. */
    fun background(coreDelayFn: (String) -> Long, direct: Boolean, coreDelay: Boolean): List<Measurement> {
        val out = mutableListOf<Measurement>()
        val guid = MmkvManager.getSelectServer() ?: return out
        val p = MmkvManager.decodeServerConfig(guid) ?: return out
        if (coreDelay) out += coreDelay(p.remarks, coreDelayFn)
        if (direct) {
            out += directServerProbes(p, 6000, tcp = true, tls = true)
            out += http("https://ya.ru/", false, 6000, maxBytes = 64 * 1024)
        }
        return out
    }
}
