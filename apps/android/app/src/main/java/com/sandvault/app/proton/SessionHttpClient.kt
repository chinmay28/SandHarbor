package com.sandvault.app.proton

import com.sandvault.app.core.proton.ProtonApi
import com.sandvault.app.core.proton.ProtonSession
import com.sandvault.app.core.proton.SessionKeeper
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import me.proton.drive.sdk.SdkHttpClient
import me.proton.drive.sdk.SdkHttpClientException
import me.proton.drive.sdk.SdkHttpRequest
import me.proton.drive.sdk.SdkHttpResponse
import okhttp3.MediaType
import okhttp3.MediaType.Companion.toMediaTypeOrNull
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import okio.BufferedSink
import okio.source
import java.io.InputStream
import java.io.InterruptedIOException
import java.io.IOException
import java.util.concurrent.TimeUnit

/**
 * The Drive SDK's HTTP, with this account's session on it.
 *
 * The SDK builds every request and reads every answer; the host's part is the
 * transport and who is asking — Proton's identification header, the session
 * UID and the access token — and refreshing that token when Proton turns it
 * down, which [SessionKeeper] does once however many requests notice.
 *
 * A request whose body is a stream (a block on its way to storage) cannot be
 * sent twice, so a 401 on one goes back to the SDK as it came; the next
 * request refreshes.
 */
class SessionHttpClient(
    private val api: ProtonApi,
    private val keeper: SessionKeeper,
    private val current: () -> ProtonSession,
    private val http: OkHttpClient = shared,
) : SdkHttpClient {

    override suspend fun execute(request: SdkHttpRequest): SdkHttpResponse = withContext(Dispatchers.IO) {
        val streamed = request.contentLength < 0 && request.body != null
        val buffered: ByteArray? = if (streamed) null else request.body?.use(InputStream::readBytes)

        val first = current()
        val answer = send(request, first, buffered)
        if (answer.code != 401 || streamed) return@withContext answer.toSdk()

        answer.close()
        val fresh = runCatching { keeper.refreshed(first) }.getOrElse {
            throw SdkHttpClientException.Http(401, message = it.message)
        }
        send(request, fresh, buffered).toSdk()
    }

    private fun send(request: SdkHttpRequest, session: ProtonSession, buffered: ByteArray?): okhttp3.Response {
        val builder = Request.Builder().url(request.url)
        request.headers.forEach { (k, v) -> builder.header(k, v) }
        api.headers(session).forEach { (k, v) ->
            // The SDK's own values win where it set one.
            if (request.headers.keys.none { it.equals(k, ignoreCase = true) }) builder.header(k, v)
        }
        val contentType = request.headers.entries.firstOrNull { it.key.equals("Content-Type", true) }?.value
        val body: RequestBody? = when {
            buffered != null -> buffered.toRequestBody(contentType?.toMediaTypeOrNull())
            request.body != null -> StreamBody(request.body!!)
            request.method in listOf("POST", "PUT", "PATCH") -> ByteArray(0).toRequestBody(null)
            else -> null
        }
        builder.method(request.method, body)
        return try {
            http.newCall(builder.build()).execute()
        } catch (e: InterruptedIOException) {
            throw SdkHttpClientException.Timeout(e)
        } catch (e: IOException) {
            throw SdkHttpClientException.Connection(e)
        }
    }

    private fun okhttp3.Response.toSdk() = SdkHttpResponse(
        statusCode = code,
        headers = headers.toMultimap(),
        body = body?.byteStream(),
    )

    /** A one-shot body read straight from the SDK's stream, sent chunked. */
    private class StreamBody(private val input: InputStream) : RequestBody() {
        override fun contentType(): MediaType? = null
        override fun contentLength(): Long = -1
        override fun isOneShot(): Boolean = true
        override fun writeTo(sink: BufferedSink) {
            input.source().use { sink.writeAll(it) }
        }
    }

    companion object {
        val shared: OkHttpClient = OkHttpClient.Builder()
            .connectTimeout(30, TimeUnit.SECONDS)
            .readTimeout(120, TimeUnit.SECONDS)
            .writeTimeout(120, TimeUnit.SECONDS)
            .followRedirects(false)
            .build()
    }
}
