package com.v2ray.ang.handler.mgmt

import android.util.Base64
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.v2ray.ang.AppConfig
import com.v2ray.ang.util.LogUtil
import okhttp3.Call
import okhttp3.Connection
import okhttp3.EventListener
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.io.ByteArrayOutputStream
import java.net.Proxy
import java.security.MessageDigest
import java.security.SecureRandom
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.TimeUnit
import java.util.zip.GZIPOutputStream
import javax.net.ssl.SSLContext
import javax.net.ssl.X509TrustManager

/**
 * HTTPS transport to the management server.
 *
 * The server uses a long-lived self-signed certificate, so the client trusts exactly the pinned
 * SubjectPublicKeyInfo hash ("sha256/<base64>") instead of a CA chain and the host name; that
 * keeps the channel independent of certificate renewals and lets the same key answer under any
 * name or IP. With no pin configured the system trust store and normal hostname checks apply.
 *
 * Requests go to the first of [MgmtSettings.urlsToTry] that accepts a connection, so the server
 * can move to a new name or IP without a new build.
 *
 * This app's own traffic is excluded from its VPN (CoreVpnService always disallows the app's
 * package), so these requests take the physical network even when the tunnel is broken.
 */
object MgmtHttp {
    class HttpResult(val code: Int, val body: JsonObject?, val error: String?) {
        val ok get() = code in 200..299
    }

    private val clients = ConcurrentHashMap<String, OkHttpClient>()
    private val jsonType = "application/json".toMediaType()

    fun pinOf(cert: X509Certificate): String {
        val digest = MessageDigest.getInstance("SHA-256").digest(cert.publicKey.encoded)
        return "sha256/" + Base64.encodeToString(digest, Base64.NO_WRAP)
    }

    /** Set once a call holds a connection, i.e. TCP and TLS to the pinned server succeeded. */
    private class Reached {
        @Volatile
        var value = false
    }

    private val reachListener = object : EventListener() {
        override fun connectionAcquired(call: Call, connection: Connection) {
            call.request().tag(Reached::class.java)?.value = true
        }
    }

    private class PinnedTrustManager(private val pin: String) : X509TrustManager {
        override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {
            throw CertificateException("client certificates are not used")
        }

        override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
            val leaf = chain?.firstOrNull() ?: throw CertificateException("empty chain")
            val got = pinOf(leaf)
            if (got != pin) throw CertificateException("management server pin mismatch: $got")
        }

        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }

    private fun client(readTimeoutSec: Long): OkHttpClient {
        val pin = MgmtSettings.pin()
        val key = "$pin|$readTimeoutSec"
        return clients.getOrPut(key) {
            val b = OkHttpClient.Builder()
                .proxy(Proxy.NO_PROXY)
                .connectTimeout(15, TimeUnit.SECONDS)
                .writeTimeout(60, TimeUnit.SECONDS)
                .readTimeout(readTimeoutSec, TimeUnit.SECONDS)
                .retryOnConnectionFailure(true)
                .eventListener(reachListener)
            if (pin.isNotBlank()) {
                val tm = PinnedTrustManager(pin)
                val ctx = SSLContext.getInstance("TLS")
                ctx.init(null, arrayOf(tm), SecureRandom())
                b.sslSocketFactory(ctx.socketFactory, tm)
                // Identity is proven by the pinned key; the self-signed certificate names no host.
                b.hostnameVerifier { _, _ -> true }
            }
            b.build()
        }
    }

    private fun gzip(data: ByteArray): ByteArray {
        val bos = ByteArrayOutputStream()
        GZIPOutputStream(bos).use { it.write(data) }
        return bos.toByteArray()
    }

    fun post(path: String, body: JsonObject, token: String?, compress: Boolean = false, readTimeoutSec: Long = 60): HttpResult {
        val bytes = body.toString().toByteArray(Charsets.UTF_8)
        return if (compress && bytes.size > 2048) {
            val gz = gzip(bytes).toRequestBody(jsonType)
            execute(path, token, readTimeoutSec) { it.header("Content-Encoding", "gzip").post(gz) }
        } else {
            val plain = bytes.toRequestBody(jsonType)
            execute(path, token, readTimeoutSec) { it.post(plain) }
        }
    }

    /**
     * [onCall] receives each call before it starts (one per server address tried); cancelling it
     * ends the request with an error.
     */
    fun get(pathAndQuery: String, token: String?, readTimeoutSec: Long = 60, onCall: ((Call) -> Unit)? = null): HttpResult {
        return execute(pathAndQuery, token, readTimeoutSec, onCall) { it.get() }
    }

    /**
     * Tries the server addresses in turn. Only a failure before any connection was made moves on
     * to the next one: the request cannot have reached the server, so sending it again is safe.
     */
    private fun execute(
        path: String,
        token: String?,
        readTimeoutSec: Long,
        onCall: ((Call) -> Unit)? = null,
        method: (Request.Builder) -> Request.Builder,
    ): HttpResult {
        val failed = mutableListOf<Pair<String, String>>()
        for (base in MgmtSettings.urlsToTry()) {
            val reached = Reached()
            val call = try {
                val rb = method(Request.Builder().url(base + path)).tag(Reached::class.java, reached)
                if (!token.isNullOrBlank()) rb.header("Authorization", "Bearer $token")
                rb.header("User-Agent", "v2rayNG-family-mgmt")
                client(readTimeoutSec).newCall(rb.build())
            } catch (e: Exception) {
                failed += base to "${e.javaClass.simpleName}: ${e.message}"
                continue
            }
            onCall?.invoke(call)
            try {
                val result = call.execute().use { resp ->
                    val text = resp.body?.string().orEmpty()
                    val json = try {
                        if (text.isBlank()) null else JsonParser.parseString(text).asJsonObject
                    } catch (_: Exception) {
                        null
                    }
                    val err = if (resp.isSuccessful) null else (json?.get("error")?.asString ?: "HTTP ${resp.code}")
                    HttpResult(resp.code, json, err)
                }
                MgmtSettings.recordUrl(base, reached = true)
                if (failed.isNotEmpty()) {
                    val before = failed.joinToString("; ") { "${it.first}: ${it.second}" }
                    LogUtil.w(AppConfig.TAG, "Mgmt: reached the server at $base after $before")
                }
                return result
            } catch (e: Exception) {
                val msg = "${e.javaClass.simpleName}: ${e.message}"
                if (call.isCanceled() || reached.value) return HttpResult(0, null, msg)
                MgmtSettings.recordUrl(base, reached = false)
                failed += base to msg
            }
        }
        val error = when (failed.size) {
            0 -> "no management server URL"
            1 -> failed[0].second
            else -> failed.joinToString("; ") { "${it.first}: ${it.second}" }
        }
        return HttpResult(0, null, error)
    }
}
