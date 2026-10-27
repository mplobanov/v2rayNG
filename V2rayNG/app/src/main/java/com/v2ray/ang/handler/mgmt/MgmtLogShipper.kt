package com.v2ray.ang.handler.mgmt

import com.google.gson.JsonArray
import com.google.gson.JsonObject
import com.v2ray.ang.AppConfig
import com.v2ray.ang.handler.MmkvManager
import com.v2ray.ang.util.LogUtil
import java.io.BufferedReader
import java.io.InputStreamReader
import java.util.ArrayDeque
import java.util.Locale

/**
 * Ships this app's logcat lines (xray core "GoLog", the app's own tag, crashes) to the server.
 *
 * Android lets an app read only its own UID's log lines, which is exactly what is wanted here.
 * The daemon streams `logcat` continuously into a bounded buffer; the idle worker dumps what
 * accumulated since the last uploaded timestamp.
 */
class MgmtLogShipper(private val maxLines: Int) {
    data class Line(val ts: Double, val pid: Int, val lvl: String, val tag: String, val msg: String)

    private val buffer = ArrayDeque<Line>()
    private var dropped = 0
    @Volatile
    private var process: Process? = null
    @Volatile
    private var stopped = false

    companion object {
        // "  1730026112.123  1234  1250 I GoLog   : message" (logcat -v epoch)
        private val lineRe = Regex("""^\s*(\d+\.\d+)\s+(\d+)\s+\d+\s+([VDIWEFA])\s+(.*?)\s*: ?(.*)$""")

        fun parse(raw: String): Line? {
            val m = lineRe.find(raw) ?: return null
            val (ts, pid, lvl, tag, msg) = m.destructured
            return Line(ts.toDoubleOrNull() ?: return null, pid.toIntOrNull() ?: 0, lvl, tag, msg)
        }

        /**
         * Drops core DNS cache hits (one per lookup an app makes; they say nothing about the
         * resolvers) unless the server asks for them.
         */
        fun keep(line: Line, dnsCacheLines: Boolean): Boolean =
            dnsCacheLines || line.tag != "GoLog" ||
                !(line.msg.contains(" cache HIT: ") || line.msg.contains(" cache OPTIMISTE: "))

        /** Expands the server's filter spec ("*:W GoLog:V {app}:V") into logcat arguments. */
        fun specArgs(spec: String): List<String> =
            spec.replace("{app}", AppConfig.TAG).split(Regex("\\s+")).filter { it.isNotBlank() }

        fun lastUploadedTs(): Double =
            MmkvManager.decodeSettingsString(MgmtSettings.KEY_LAST_LOG_TS)?.toDoubleOrNull() ?: 0.0

        private fun startArg(sinceTs: Double): String {
            val now = System.currentTimeMillis() / 1000.0
            // Never replay more than an hour; logcat's ring rarely holds that much anyway.
            val from = if (sinceTs <= 0.0 || sinceTs < now - 3600) now - 300 else sinceTs + 0.001
            return String.format(Locale.US, "%.3f", from)
        }

        fun toJson(lines: List<Line>, dropped: Int, source: String): JsonObject = JsonObject().apply {
            add("lines", JsonArray().also { arr ->
                lines.forEach { l ->
                    arr.add(JsonObject().apply {
                        addProperty("ts", l.ts)
                        addProperty("pid", l.pid)
                        addProperty("lvl", l.lvl)
                        addProperty("tag", l.tag)
                        addProperty("msg", l.msg)
                    })
                }
            })
            addProperty("dropped", dropped)
            addProperty("source", source)
        }

        /** One-shot read of the log buffer (idle worker, upload_logs command). */
        fun dump(spec: String, sinceTs: Double?, maxLines: Int = 50_000, dnsCacheLines: Boolean = false): List<Line> {
            val args = mutableListOf("logcat", "-d", "-v", "epoch")
            if (sinceTs != null) {
                args += listOf("-T", startArg(sinceTs))
            }
            args += specArgs(spec)
            return try {
                val p = Runtime.getRuntime().exec(args.toTypedArray())
                val out = ArrayDeque<Line>()
                BufferedReader(InputStreamReader(p.inputStream)).useLines { seq ->
                    seq.forEach { raw ->
                        parse(raw)?.takeIf { keep(it, dnsCacheLines) }?.let {
                            out.addLast(it)
                            if (out.size > maxLines) out.removeFirst()
                        }
                    }
                }
                p.waitFor()
                out.toList()
            } catch (e: Exception) {
                LogUtil.e(AppConfig.TAG, "Mgmt: logcat dump failed", e)
                emptyList()
            }
        }

        fun upload(lines: List<Line>, dropped: Int, source: String): Boolean {
            if (lines.isEmpty() && dropped == 0) return true
            val token = MgmtSettings.deviceToken() ?: return false
            val res = MgmtHttp.post("/v1/logs", toJson(lines, dropped, source), token, compress = true)
            if (res.ok && lines.isNotEmpty()) {
                val maxTs = lines.maxOf { it.ts }
                if (maxTs > lastUploadedTs()) {
                    MmkvManager.encodeSettings(MgmtSettings.KEY_LAST_LOG_TS, String.format(Locale.US, "%.6f", maxTs))
                }
            }
            return res.ok
        }
    }

    /** Blocking: streams logcat until [stop]. Run it on a dedicated IO thread. */
    fun stream(spec: String, dnsCacheLines: Boolean = false) {
        val args = mutableListOf("logcat", "-v", "epoch", "-T", startArg(lastUploadedTs())) + specArgs(spec)
        while (!stopped) {
            try {
                val p = Runtime.getRuntime().exec(args.toTypedArray())
                process = p
                BufferedReader(InputStreamReader(p.inputStream)).use { r ->
                    while (!stopped) {
                        val raw = r.readLine() ?: break
                        val line = parse(raw)?.takeIf { keep(it, dnsCacheLines) } ?: continue
                        synchronized(buffer) {
                            buffer.addLast(line)
                            while (buffer.size > maxLines) {
                                buffer.removeFirst()
                                dropped++
                            }
                        }
                    }
                }
                p.destroy()
            } catch (e: Exception) {
                if (!stopped) LogUtil.e(AppConfig.TAG, "Mgmt: logcat stream failed", e)
            }
            if (!stopped) Thread.sleep(5_000)
        }
    }

    fun stop() {
        stopped = true
        process?.destroy()
    }

    /** Uploads buffered lines in batches; lines stay buffered when the upload fails. */
    fun flush(): Boolean {
        while (true) {
            val batch: List<Line>
            val drop: Int
            synchronized(buffer) {
                if (buffer.isEmpty() && dropped == 0) return true
                batch = buffer.take(5000)
                drop = dropped
            }
            if (!upload(batch, drop, "stream")) return false
            synchronized(buffer) {
                repeat(minOf(batch.size, buffer.size)) { buffer.removeFirst() }
                dropped -= drop
            }
            if (batch.size < 5000) return true
        }
    }
}
