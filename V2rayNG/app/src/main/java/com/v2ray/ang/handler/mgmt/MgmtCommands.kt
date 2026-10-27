package com.v2ray.ang.handler.mgmt

import android.content.Context
import com.google.gson.JsonArray
import com.google.gson.JsonObject
import com.v2ray.ang.AppConfig
import com.v2ray.ang.core.LauncherManager
import com.v2ray.ang.dto.UrlContentRequest
import com.v2ray.ang.dto.entities.SubscriptionCache
import com.v2ray.ang.handler.AngConfigManager
import com.v2ray.ang.handler.MmkvManager
import com.v2ray.ang.handler.SettingsManager
import com.v2ray.ang.util.HttpUtil
import com.v2ray.ang.util.LogUtil
import com.v2ray.ang.util.Utils
import java.io.File
import java.util.concurrent.TimeUnit

/**
 * Executes commands queued on the management server (see API.md "Commands").
 * Runs on an IO thread; returns the result object posted back to the server.
 */
object MgmtCommands {
    /** What the executing process can offer: the daemon has the running core, the worker has not. */
    class Env(
        val context: Context,
        val coreRunning: Boolean,
        val coreDelay: ((String) -> Long)?,
        val requestSync: (full: Boolean) -> Unit,
    )

    private val execAllowed = setOf("ping", "ping6", "getprop", "ip", "cat", "ls")

    fun execute(env: Env, cmd: JsonObject): JsonObject {
        val id = cmd.get("id").asLong
        val type = cmd.get("type")?.asString.orEmpty()
        val args = cmd.get("args")?.takeIf { it.isJsonObject }?.asJsonObject ?: JsonObject()
        val out = JsonObject().apply { addProperty("id", id) }
        LogUtil.i(AppConfig.TAG, "Mgmt: command #$id $type")
        try {
            val result = JsonObject()
            val measurements = JsonArray()
            when (type) {
                "sync" -> env.requestSync(false)
                "dump_state" -> env.requestSync(true)
                "upload_logs" -> {
                    val spec = args.get("spec")?.asString ?: MgmtSettings.agentString("log_spec", "*:W GoLog:V {app}:V")
                    val lines = MgmtLogShipper.dump(spec, sinceTs = null)
                    val ok = lines.chunked(5000).all { MgmtLogShipper.upload(it, 0, "dump") }
                    result.addProperty("lines", lines.size)
                    if (!ok) throw IllegalStateException("upload failed")
                }
                "probe" -> {
                    MgmtProbes.run(env.context, args, env.coreRunning, env.coreDelay).forEach { measurements.add(it.toJson()) }
                    val okCount = measurements.count { it.asJsonObject.get("ok").asBoolean }
                    result.addProperty("summary", "$okCount/${measurements.size()} ok")
                    result.add("measurements", measurements)
                }
                "restart_vpn" -> LauncherManager.restartService(env.context)
                "stop_vpn" -> LauncherManager.stopService(env.context)
                "start_vpn" -> LauncherManager.startService(env.context)
                "update_subscriptions" -> {
                    val only = args.get("id")?.asString
                    val subs = MmkvManager.decodeSubscriptions().filter { only == null || it.guid == only || it.guid == MgmtConfigApplier.managedSubGuid(only) }
                    subs.forEach { s: SubscriptionCache ->
                        val r = AngConfigManager.updateConfigViaSub(s)
                        result.addProperty(s.subscription.remarks, "configs=${r.configCount} ok=${r.successCount} fail=${r.failureCount}")
                    }
                }
                "update_geo" -> updateGeo(env.context, result)
                "select_server" -> {
                    val remarks = args.get("remarks")?.asString ?: throw IllegalArgumentException("remarks required")
                    val guid = MgmtConfigApplier.findServer(remarks, args.get("subscription")?.asString)
                        ?: throw IllegalArgumentException("no server named \"$remarks\"")
                    MmkvManager.setSelectServer(guid)
                    result.addProperty("selected", guid)
                    if (env.coreRunning) LauncherManager.restartService(env.context)
                }
                "exec" -> exec(args, result)
                "reenroll" -> {
                    MgmtSettings.forgetEnrollment()
                    env.requestSync(true)
                }
                else -> throw IllegalArgumentException("unsupported command $type")
            }
            out.addProperty("ok", true)
            out.add("result", result)
            if (measurements.size() > 0) out.add("measurements", measurements)
        } catch (e: Exception) {
            LogUtil.e(AppConfig.TAG, "Mgmt: command #$id $type failed", e)
            out.addProperty("ok", false)
            out.addProperty("error", "${e.javaClass.simpleName}: ${e.message}")
        }
        return out
    }

    private fun updateGeo(context: Context, result: JsonObject) {
        val source = MmkvManager.decodeSettingsString(AppConfig.PREF_GEO_FILES_SOURCES) ?: AppConfig.GEO_FILES_SOURCES.first()
        val dir = File(Utils.userAssetPath(context))
        listOf(AppConfig.GEOSITE_DAT, AppConfig.GEOIP_DAT).forEach { name ->
            val url = String.format(AppConfig.GITHUB_DOWNLOAD_URL, source) + "/" + name
            val tmp = File(dir, "${name}_temp")
            // Through the running core first (GitHub can be slow from Russia), then directly.
            val ok = listOf(SettingsManager.getHttpPort(), 0).any { port ->
                HttpUtil.downloadToFile(UrlContentRequest(url = url, timeout = 60000, httpPort = port), tmp)
            }
            if (ok && tmp.length() > 0) {
                tmp.renameTo(File(dir, name))
                result.addProperty(name, "ok ${File(dir, name).length()} bytes")
            } else {
                tmp.delete()
                result.addProperty(name, "failed")
            }
        }
    }

    private fun exec(args: JsonObject, result: JsonObject) {
        val argv = args.getAsJsonArray("argv")?.map { it.asString } ?: throw IllegalArgumentException("argv required")
        require(argv.isNotEmpty() && argv[0] in execAllowed) { "only ${execAllowed.joinToString()} are allowed" }
        if (argv[0] == "cat" || argv[0] == "ls") {
            require(argv.drop(1).filterNot { it.startsWith("-") }.all { it.startsWith("/proc/") }) { "${argv[0]} is limited to /proc" }
        }
        val timeout = args.get("timeout_sec")?.asLong ?: 20L
        val p = ProcessBuilder(argv).redirectErrorStream(true).start()
        val output = StringBuilder()
        val reader = Thread {
            p.inputStream.bufferedReader().useLines { seq ->
                seq.forEach { if (output.length < 64_000) output.appendLine(it) }
            }
        }.apply { start() }
        // Process.waitFor(timeout) is API 26; minSdk is 24, so wait on a helper thread instead.
        val waiter = Thread {
            try {
                p.waitFor()
            } catch (_: InterruptedException) {
            }
        }.apply { start() }
        waiter.join(TimeUnit.SECONDS.toMillis(timeout))
        val finished = !waiter.isAlive
        if (!finished) p.destroy()
        reader.join(2000)
        result.addProperty("exit", if (finished) p.exitValue() else -1)
        result.addProperty("timed_out", !finished)
        result.addProperty("output", output.toString())
    }
}
