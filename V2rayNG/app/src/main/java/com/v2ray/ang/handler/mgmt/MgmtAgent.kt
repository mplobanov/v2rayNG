package com.v2ray.ang.handler.mgmt

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import com.google.gson.JsonObject
import com.v2ray.ang.AppConfig
import com.v2ray.ang.core.CoreNativeManager
import com.v2ray.ang.handler.MmkvManager
import com.v2ray.ang.util.LogUtil
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import java.util.concurrent.ConcurrentLinkedQueue

/**
 * Remote-management agent of the VPN daemon process. It lives exactly as long as the core runs:
 * CoreServiceManager starts it after the core is up and stops it during teardown.
 *
 * Loops: sync + long-poll for commands, logcat streaming and upload, periodic health probes,
 * and network-change tracking. While the VPN is off, MgmtSyncWorker does the (slower) syncing.
 */
object MgmtAgent {
    /** What the agent needs from the running core; supplied by CoreServiceManager. */
    interface CoreAccess {
        fun isRunning(): Boolean
        fun startedAtMs(): Long
        fun traffic(): Map<String, Long>
        fun measureDelay(url: String): Long
    }

    private var scope: CoroutineScope? = null
    private var context: Context? = null
    private var core: CoreAccess? = null
    private var shipper: MgmtLogShipper? = null
    private var netCallback: ConnectivityManager.NetworkCallback? = null
    private var netFollowUp: Job? = null
    private val measurements = ConcurrentLinkedQueue<JsonObject>()
    private val results = ConcurrentLinkedQueue<JsonObject>()
    private var wake = Channel<Boolean>(Channel.CONFLATED)
    private var commandQueue = Channel<JsonObject>(Channel.UNLIMITED)

    private fun facts(): MgmtTelemetry.CoreFacts {
        val c = core
        return if (c == null) {
            MgmtTelemetry.CoreFacts(running = false, inDaemon = true)
        } else {
            val running = try { c.isRunning() } catch (_: Exception) { false }
            MgmtTelemetry.CoreFacts(running, inDaemon = true, startedAtMs = c.startedAtMs(),
                traffic = if (running) try { c.traffic() } catch (_: Exception) { emptyMap() } else emptyMap())
        }
    }

    @Synchronized
    fun start(context: Context, access: CoreAccess) {
        if (!MgmtSettings.isEnabled() || scope != null) return
        val ctx = context.applicationContext
        val s = CoroutineScope(SupervisorJob() + Dispatchers.IO)
        scope = s
        this.context = ctx
        core = access
        wake = Channel(Channel.CONFLATED)
        commandQueue = Channel(Channel.UNLIMITED)
        MgmtSettings.markAgentAlive(true)

        val sh = MgmtLogShipper(MgmtSettings.agentInt("log_buffer_lines", 20000))
        shipper = sh
        s.launch {
            MmkvManager.encodeSettings(MgmtSettings.KEY_CORE_VERSION, CoreNativeManager.getLibVersion())
        }
        s.launch { sh.stream(MgmtSettings.agentString("log_spec", "*:W GoLog:V {app}:V")) }
        s.launch { syncLoop(ctx) }
        s.launch { commandLoop(ctx) }
        s.launch { logLoop() }
        s.launch { probeLoop() }
        registerNetworkCallback(ctx)
        LogUtil.i(AppConfig.TAG, "Mgmt: agent started")
    }

    /** Stops the agent after a bounded final upload (VPN stop event, last log lines). */
    @Synchronized
    fun stop() {
        val s = scope ?: return
        scope = null
        val ctx = context
        val sh = shipper
        shipper = null
        unregisterNetworkCallback(ctx)
        MgmtSettings.markAgentAlive(false)
        core = null
        s.launch {
            withTimeoutOrNull(6_000) {
                sh?.flush()
                if (ctx != null) {
                    val evs = MgmtEvents.drain()
                    val out = MgmtClient.sync(ctx, MgmtTelemetry.CoreFacts(running = false, inDaemon = true), evs, drainAll(measurements), drainAll(results))
                    if (!out.ok) MgmtEvents.requeue(evs)
                }
            }
            sh?.stop()
            s.cancel()
        }
        LogUtil.i(AppConfig.TAG, "Mgmt: agent stopping")
    }

    /** Asks the agent for an immediate sync (no-op when it is not running). */
    fun requestSync(full: Boolean) {
        wake.trySend(full)
    }

    private fun <T> drainAll(q: ConcurrentLinkedQueue<T>): List<T> {
        val out = mutableListOf<T>()
        while (true) out.add(q.poll() ?: break)
        return out
    }

    private fun env(ctx: Context) = MgmtCommands.Env(
        context = ctx,
        coreRunning = facts().running,
        coreDelay = core?.let { c -> { url: String -> c.measureDelay(url) } },
        requestSync = { full -> requestSync(full) },
    )

    private suspend fun CoroutineScope.syncLoop(ctx: Context) {
        var full = true
        var backoff = 15_000L
        while (isActive) {
            val evs = MgmtEvents.drain()
            val ms = drainAll(measurements)
            val rs = drainAll(results)
            val out = MgmtClient.sync(ctx, facts(), evs, ms, rs, full)
            MgmtSettings.markAgentAlive(true)
            if (!out.ok) {
                MgmtEvents.requeue(evs)
                measurements.addAll(ms.takeLast(500))
                results.addAll(rs)
                LogUtil.w(AppConfig.TAG, "Mgmt: sync failed: ${out.error}")
                val wakeReq = withTimeoutOrNull(backoff) { wake.receive() }
                if (wakeReq != null) full = full || wakeReq
                backoff = (backoff * 2).coerceAtMost(300_000L)
                continue
            }
            full = false
            backoff = 15_000L
            out.commands.forEach { commandQueue.trySend(it) }
            if (out.status != "approved") {
                // Pending approval: check back every minute so approval takes effect quickly.
                val wakeReq = withTimeoutOrNull(60_000L) { wake.receive() }
                if (wakeReq != null) full = wakeReq
                continue
            }
            full = waitForNextSync()
        }
    }

    /**
     * Long-polls between syncs so commands and config changes arrive within seconds.
     * Returns whether the next sync should upload the full state.
     */
    private suspend fun CoroutineScope.waitForNextSync(): Boolean {
        val deadline = System.currentTimeMillis() + MgmtSettings.agentInt("sync_sec", 300) * 1000L
        while (isActive) {
            wake.tryReceive().getOrNull()?.let { return it }
            val remaining = deadline - System.currentTimeMillis()
            if (remaining <= 0) return false
            val wait = minOf(MgmtSettings.agentInt("poll_wait_sec", 240).toLong(), remaining / 1000).coerceAtLeast(5)
            val r = MgmtClient.poll(wait.toInt())
            MgmtSettings.markAgentAlive(true)
            if (r == null || !r.ok || r.body == null) {
                val wakeReq = withTimeoutOrNull(minOf(30_000L, remaining.coerceAtLeast(1000))) { wake.receive() }
                if (wakeReq != null) return wakeReq
                continue
            }
            val body = r.body
            body.getAsJsonObject("agent")?.let { MmkvManager.encodeSettings(MgmtSettings.KEY_AGENT_PARAMS, it.toString()) }
            body.get("commands")?.takeIf { it.isJsonArray }?.asJsonArray?.forEach {
                if (it.isJsonObject) commandQueue.trySend(it.asJsonObject)
            }
            if (body.get("config_changed")?.asBoolean == true) return false
            if (body.get("status")?.asString.let { it != null && it != "approved" }) return false
        }
        return false
    }

    private suspend fun commandLoop(ctx: Context) {
        for (cmd in commandQueue) {
            val result = MgmtCommands.execute(env(ctx), cmd)
            if (!MgmtClient.postResults(listOf(result))) results.add(result)
        }
    }

    private suspend fun CoroutineScope.logLoop() {
        while (isActive) {
            val sec = if (MgmtSettings.isLive()) 3 else MgmtSettings.agentInt("log_upload_sec", 60)
            delay(sec.coerceAtLeast(2) * 1000L)
            try {
                shipper?.flush()
            } catch (e: Exception) {
                LogUtil.e(AppConfig.TAG, "Mgmt: log upload failed", e)
            }
        }
    }

    private suspend fun CoroutineScope.probeLoop() {
        delay(45_000)
        while (isActive) {
            val sec = MgmtSettings.agentInt("probe_sec", 900)
            if (sec > 0) runBackgroundProbe()
            delay(if (sec > 0) sec * 1000L else 300_000L)
        }
    }

    private fun runBackgroundProbe() {
        val c = core ?: return
        if (MgmtSettings.status() != "approved" || !facts().running) return
        try {
            MgmtProbes.background(
                coreDelayFn = { url -> c.measureDelay(url) },
                direct = MgmtSettings.agentBool("probe_direct", true),
                coreDelay = MgmtSettings.agentBool("probe_core_delay", true),
            ).forEach { measurements.add(it.toJson()) }
        } catch (e: Exception) {
            LogUtil.e(AppConfig.TAG, "Mgmt: background probe failed", e)
        }
    }

    private fun netSignature(cm: ConnectivityManager, network: Network): String {
        val caps = try { cm.getNetworkCapabilities(network) } catch (_: Exception) { null }
        val t = buildList {
            if (caps?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true) add("wifi")
            if (caps?.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) == true) add("cellular")
            if (caps?.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) == true) add("ethernet")
        }
        val validated = caps?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true
        return "$network/${t.joinToString(",")}/validated=$validated"
    }

    private fun registerNetworkCallback(ctx: Context) {
        val cm = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager ?: return
        var last = ""
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) = report("net_available", network)
            override fun onCapabilitiesChanged(network: Network, networkCapabilities: NetworkCapabilities) = report("net_change", network)
            override fun onLost(network: Network) {
                last = ""
                MgmtEvents.add("net_lost", JsonObject().apply { addProperty("id", network.toString()) })
            }

            private fun report(type: String, network: Network) {
                val sig = netSignature(cm, network)
                if (sig == last) return
                last = sig
                MgmtEvents.add(type, MgmtTelemetry.networkSummary(ctx, cm, network))
                // Measure the new network once it settled, then sync so the change is visible.
                netFollowUp?.cancel()
                netFollowUp = scope?.launch {
                    delay(8_000)
                    runBackgroundProbe()
                    requestSync(false)
                }
            }
        }
        try {
            // This app is excluded from its own VPN, so its default network is the physical one.
            cm.registerDefaultNetworkCallback(cb)
            netCallback = cb
        } catch (e: Exception) {
            LogUtil.e(AppConfig.TAG, "Mgmt: network callback registration failed", e)
        }
    }

    private fun unregisterNetworkCallback(ctx: Context?) {
        val cb = netCallback ?: return
        netCallback = null
        netFollowUp?.cancel()
        try {
            (ctx?.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager)?.unregisterNetworkCallback(cb)
        } catch (_: Exception) {
        }
    }
}
