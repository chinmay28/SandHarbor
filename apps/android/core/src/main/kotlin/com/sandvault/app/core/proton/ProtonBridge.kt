package com.sandvault.app.core.proton

import fi.iki.elonen.NanoHTTPD
import org.json.JSONArray
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.io.InputStream
import java.security.MessageDigest
import java.security.SecureRandom
import java.util.Base64

/** One stored part, as the vault knows it. */
data class StoredPart(val key: String, val size: Long)

/** An account request: whose session, which folder, and room for a refreshed session. */
class AccountCall(val session: ProtonSession, val folder: String) {
    /** Set by the store when it had to refresh; the bridge hands it back to the vault. */
    @Volatile
    var refreshed: ProtonSession? = null
}

/**
 * Proton Drive as the bridge needs it: one folder of parts. The app's
 * implementation is the Drive SDK (app/.../proton/SdkProtonStore.kt); a test's
 * is a map.
 *
 * Failures are [BridgeFailure]s; anything else thrown is reported as internal.
 */
interface ProtonStore {
    /** The session is good and the folder exists, made if it was not. */
    fun ping(call: AccountCall)

    fun email(call: AccountCall): String

    /** Bytes used and the plan's size, as Proton reports them. */
    fun usage(call: AccountCall): Pair<Long, Long>

    fun list(call: AccountCall, prefix: String): List<StoredPart>

    fun get(call: AccountCall, key: String): ByteArray

    fun stat(call: AccountCall, key: String): StoredPart

    /** Stores the part, replacing one of the same key, and returns once Proton has it. */
    fun put(call: AccountCall, key: String, data: ByteArray)

    /** Removes the part; [BridgeFailure.NotFound] if it was not there. */
    fun delete(call: AccountCall, key: String)
}

/** The failures the vault distinguishes; see the protocol in docs/design/proton-android.md. */
sealed class BridgeFailure(val status: Int, val code: String, message: String) : Exception(message) {
    class NotFound(message: String = "no such part") : BridgeFailure(404, "not_found", message)
    class SignedOut(message: String) : BridgeFailure(401, "signed_out", message)
    class BadRequest(message: String) : BridgeFailure(400, "bad_request", message)
    class Proton(message: String) : BridgeFailure(502, "proton", message)
}

/** How the server finds the bridge: put into its environment by the app. */
data class BridgeEndpoint(val url: String, val token: String) {
    fun environment(): Map<String, String> = mapOf(ENV_URL to url, ENV_TOKEN to token)

    companion object {
        const val ENV_URL = "SAND_APP_BRIDGE"
        const val ENV_TOKEN = "SAND_APP_BRIDGE_TOKEN"

        fun newToken(random: SecureRandom = SecureRandom()): String =
            Base64.getUrlEncoder().withoutPadding().encodeToString(ByteArray(32).also(random::nextBytes))
    }
}

/**
 * The bridge: an HTTP server on loopback that the vault's server, a child
 * process of the app, asks to reach Proton. Every request must carry the
 * token the app gave that server; loopback is open to every app on the phone.
 *
 * Port 0 lets the system pick one; [endpoint] says which it picked.
 */
class ProtonBridge(
    private val store: ProtonStore,
    private val signIns: SignInFlows,
    private val token: String = BridgeEndpoint.newToken(),
    port: Int = 0,
    private val longPollMs: Long = 25_000,
) : NanoHTTPD("127.0.0.1", port) {

    private val tokenDigest = digest(token)

    fun begin(): BridgeEndpoint {
        start(SOCKET_READ_TIMEOUT, true)
        return endpoint()
    }

    fun endpoint(): BridgeEndpoint = BridgeEndpoint("http://127.0.0.1:$listeningPort", token)

    override fun serve(session: IHTTPSession): Response = try {
        if (!authorized(session.headers["authorization"])) {
            json(401, JSONObject().put("error", "not the app's server").put("code", "bad_request"))
        } else {
            route(session)
        }
    } catch (f: BridgeFailure) {
        json(f.status, JSONObject().put("error", f.message).put("code", f.code))
    } catch (e: ProtonSignedOut) {
        json(401, JSONObject().put("error", e.message).put("code", "signed_out"))
    } catch (e: ProtonRefused) {
        json(502, JSONObject().put("error", e.message).put("code", "proton"))
    } catch (e: Exception) {
        json(500, JSONObject().put("error", e.message ?: e.javaClass.simpleName).put("code", "internal"))
    }

    private fun authorized(header: String?): Boolean {
        val presented = header?.removePrefix("Bearer ")?.trim() ?: return false
        return MessageDigest.isEqual(digest(presented), tokenDigest)
    }

    private fun route(s: IHTTPSession): Response {
        val path = s.uri
        if (path == "/proton/v1/signin" && s.method == Method.POST) {
            // The body says nothing, but it must still be read: left on a
            // kept-alive connection it becomes the start of the next request.
            drain(s)
            val started = signIns.start()
            return json(200, JSONObject().put("id", started.id).put("url", started.url))
        }
        if (path.startsWith("/proton/v1/signin/") && s.method == Method.GET) {
            val id = path.removePrefix("/proton/v1/signin/")
            val state = signIns.await(id, longPollMs) ?: throw BridgeFailure.NotFound("no such sign-in")
            return json(200, when (state) {
                SignInState.Pending -> JSONObject().put("state", "pending")
                is SignInState.Done -> JSONObject().put("state", "done").put("session", state.session.toJson()).put("email", state.session.email)
                is SignInState.Failed -> JSONObject().put("state", "failed").put("error", state.reason)
            })
        }

        val call = accountCall(s)
        val reply = when {
            path == "/proton/v1/ping" -> store.ping(call).let { empty() }
            path == "/proton/v1/account" -> json(200, JSONObject().put("email", store.email(call)))
            path == "/proton/v1/usage" -> store.usage(call).let { (used, total) ->
                json(200, JSONObject().put("used", used).put("total", total))
            }
            path == "/proton/v1/objects" && s.method == Method.GET -> {
                val prefix = s.parameters["prefix"]?.firstOrNull().orEmpty()
                val out = JSONArray()
                store.list(call, prefix).forEach { out.put(JSONObject().put("key", it.key).put("size", it.size)) }
                text(200, out.toString(), "application/json")
            }
            path.startsWith(OBJECTS) -> objectRequest(s, call, key(path))
            else -> throw BridgeFailure.BadRequest("no such endpoint: ${s.method} $path")
        }
        call.refreshed?.takeIf { it != call.session }?.let { reply.addHeader(SESSION_HEADER, it.toHeader()) }
        return reply
    }

    private fun objectRequest(s: IHTTPSession, call: AccountCall, key: String): Response = when (s.method) {
        Method.PUT -> {
            store.put(call, key, body(s))
            empty()
        }
        Method.DELETE -> {
            store.delete(call, key)
            empty()
        }
        Method.GET -> if (s.parameters["stat"]?.firstOrNull() == "1") {
            store.stat(call, key).let { json(200, JSONObject().put("key", it.key).put("size", it.size)) }
        } else {
            val data = store.get(call, key)
            newFixedLengthResponse(Response.Status.OK, "application/octet-stream", ByteArrayInputStream(data), data.size.toLong())
        }
        else -> throw BridgeFailure.BadRequest("${s.method} is not something a part can have done to it")
    }

    private fun accountCall(s: IHTTPSession): AccountCall {
        val session = ProtonSession.fromHeader(s.headers[SESSION_HEADER.lowercase()])
            .getOrElse { throw BridgeFailure.BadRequest(it.message ?: "no session") }
        val folder = s.headers[FOLDER_HEADER.lowercase()]?.trim().orEmpty()
        if (!folder.startsWith("/") || folder.length < 2) throw BridgeFailure.BadRequest("no folder named")
        return AccountCall(session, folder)
    }

    private fun key(path: String): String {
        // NanoHTTPD hands over the path decoded; a key never has a slash in it.
        val key = path.removePrefix(OBJECTS)
        if (key.isEmpty() || key.contains('/') || key == "." || key == "..") throw BridgeFailure.BadRequest("not a part's key: $key")
        return key
    }

    private fun body(s: IHTTPSession): ByteArray {
        val length = s.headers["content-length"]?.toLongOrNull() ?: throw BridgeFailure.BadRequest("a part needs a length")
        if (length > MAX_PART) throw BridgeFailure.BadRequest("a part of $length bytes is larger than any SAND makes")
        return readExactly(s.inputStream, length.toInt())
    }

    /** Reads and drops a body nobody needs; see the sign-in route. */
    private fun drain(s: IHTTPSession) {
        val length = s.headers["content-length"]?.toLongOrNull() ?: return
        if (length in 1..MAX_DRAIN) readExactly(s.inputStream, length.toInt())
    }

    private fun empty(): Response = newFixedLengthResponse(Response.Status.NO_CONTENT, "text/plain", "")

    private fun json(status: Int, o: JSONObject): Response = text(status, o.toString(), "application/json")

    private fun text(status: Int, body: String, mime: String): Response =
        newFixedLengthResponse(statusOf(status), mime, body)

    private fun statusOf(code: Int): Response.IStatus =
        Response.Status.lookup(code) ?: object : Response.IStatus {
            override fun getDescription() = "$code"
            override fun getRequestStatus() = code
        }

    companion object {
        const val SESSION_HEADER = "X-Sand-Proton-Session"
        const val FOLDER_HEADER = "X-Sand-Proton-Folder"
        private const val OBJECTS = "/proton/v1/objects/"

        /** Larger than the largest part the vault cuts (64 MiB chunks). */
        private const val MAX_PART = 256L shl 20

        /** A body the bridge ignores is a few bytes of JSON; anything bigger is refused unread. */
        private const val MAX_DRAIN = 64L shl 10

        private fun digest(s: String): ByteArray = MessageDigest.getInstance("SHA-256").digest(s.toByteArray())

        internal fun readExactly(input: InputStream, n: Int): ByteArray {
            val out = ByteArray(n)
            var read = 0
            while (read < n) {
                val r = input.read(out, read, n - read)
                if (r < 0) throw BridgeFailure.BadRequest("the part arrived short: $read of $n bytes")
                read += r
            }
            return out
        }
    }
}
