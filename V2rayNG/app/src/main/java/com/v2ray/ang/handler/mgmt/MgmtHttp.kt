package com.v2ray.ang.handler.mgmt

import android.util.Base64
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import okhttp3.Call
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.io.ByteArrayOutputStream
import java.io.IOException
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
 * The server uses a long-lived self-signed certificate addressed by IP, so the client trusts
 * exactly the pinned SubjectPublicKeyInfo hash ("sha256/<base64>") instead of a CA chain; that
 * keeps the channel independent of DNS and of certificate renewals. With no pin configured the
 * system trust store and normal hostname checks apply.
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
            if (pin.isNotBlank()) {
                val tm = PinnedTrustManager(pin)
                val ctx = SSLContext.getInstance("TLS")
                ctx.init(null, arrayOf(tm), SecureRandom())
                b.sslSocketFactory(ctx.socketFactory, tm)
                // Identity is proven by the pinned key; the URL usually holds a bare IP.
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
        val rb = Request.Builder().url(MgmtSettings.url() + path)
        if (compress && bytes.size > 2048) {
            rb.header("Content-Encoding", "gzip").post(gzip(bytes).toRequestBody(jsonType))
        } else {
            rb.post(bytes.toRequestBody(jsonType))
        }
        return execute(rb, token, readTimeoutSec)
    }

    /** [onCall] receives the call before it starts; cancelling it ends the request with an error. */
    fun get(pathAndQuery: String, token: String?, readTimeoutSec: Long = 60, onCall: ((Call) -> Unit)? = null): HttpResult {
        return execute(Request.Builder().url(MgmtSettings.url() + pathAndQuery).get(), token, readTimeoutSec, onCall)
    }

    private fun execute(rb: Request.Builder, token: String?, readTimeoutSec: Long, onCall: ((Call) -> Unit)? = null): HttpResult {
        if (!token.isNullOrBlank()) rb.header("Authorization", "Bearer $token")
        rb.header("User-Agent", "v2rayNG-family-mgmt")
        return try {
            val call = client(readTimeoutSec).newCall(rb.build())
            onCall?.invoke(call)
            call.execute().use { resp ->
                val text = resp.body?.string().orEmpty()
                val json = try {
                    if (text.isBlank()) null else JsonParser.parseString(text).asJsonObject
                } catch (_: Exception) {
                    null
                }
                val err = if (resp.isSuccessful) null else (json?.get("error")?.asString ?: "HTTP ${resp.code}")
                HttpResult(resp.code, json, err)
            }
        } catch (e: IOException) {
            HttpResult(0, null, "${e.javaClass.simpleName}: ${e.message}")
        } catch (e: Exception) {
            HttpResult(0, null, "${e.javaClass.simpleName}: ${e.message}")
        }
    }
}
