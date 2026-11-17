package com.sandvault.app.core.proton

import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URI

/** One HTTP exchange, so Proton can be stood in for in a test. */
fun interface HttpTransport {
    fun execute(method: String, url: String, headers: Map<String, String>, body: ByteArray?): HttpReply
}

class HttpReply(val status: Int, val body: ByteArray, val headers: Map<String, String> = emptyMap()) {
    val text: String get() = String(body, Charsets.UTF_8)
}

/** java.net, which both the JVM and Android have. */
class UrlConnectionTransport(private val connectMs: Int = 15_000, private val readMs: Int = 60_000) : HttpTransport {
    override fun execute(method: String, url: String, headers: Map<String, String>, body: ByteArray?): HttpReply {
        val conn = URI(url).toURL().openConnection() as HttpURLConnection
        try {
            conn.requestMethod = method
            conn.connectTimeout = connectMs
            conn.readTimeout = readMs
            conn.instanceFollowRedirects = false
            headers.forEach { (k, v) -> conn.setRequestProperty(k, v) }
            if (body != null) {
                conn.doOutput = true
                conn.setFixedLengthStreamingMode(body.size)
                conn.outputStream.use { it.write(body) }
            }
            val status = conn.responseCode
            val stream = if (status >= 400) conn.errorStream else conn.inputStream
            val data = stream?.use { it.readBytes() } ?: ByteArray(0)
            val replyHeaders = conn.headerFields.filterKeys { it != null }.mapValues { it.value.joinToString(",") }
            return HttpReply(status, data, replyHeaders)
        } finally {
            conn.disconnect()
        }
    }
}

/** Proton turned the session down: it is revoked, or its refresh token is spent or expired. */
class ProtonSignedOut(message: String) : Exception(message)

/** Proton answered with an error it means. */
class ProtonRefused(val status: Int, val code: Int, message: String) : Exception(message)

/** An access token Proton no longer takes; the caller may refresh and try again. */
class ProtonUnauthorized : Exception("access token rejected")

data class ForkInit(val userCode: String, val selector: String)

/** What an approved fork hands over: a session, and the payload holding the key password. */
data class ForkApproved(val uid: String, val accessToken: String, val refreshToken: String, val payload: String)

data class ArmoredKey(val id: String, val privateKey: String, val primary: Boolean, val active: Boolean)

data class ProtonUser(
    val id: String,
    val name: String,
    val email: String,
    val usedSpace: Long,
    val maxSpace: Long,
    val keys: List<ArmoredKey>,
)

data class AddressKey(
    val id: String,
    val privateKey: String,
    /** Encrypted to the user key; its plaintext is the address key's passphrase. Empty on old accounts. */
    val token: String,
    val signature: String,
    val primary: Boolean,
    val active: Boolean,
    /** Proton's key flags: bit 0 can verify, bit 1 can encrypt. */
    val flags: Int,
)

data class ProtonAddress(
    val id: String,
    val email: String,
    val order: Int,
    /** 0 disabled, 1 enabled, 2 deleting. */
    val status: Int,
    val keys: List<AddressKey>,
)

/**
 * The handful of Proton account endpoints the app needs, as Proton's own
 * account module uses them (sdk/incubating/account/js). Drive itself is the
 * SDK's business; this is signing in, keeping the session alive, and the
 * user's keys and addresses that the SDK asks for.
 */
class ProtonApi(
    private val appVersion: String,
    private val baseUrl: String = DEFAULT_BASE_URL,
    private val http: HttpTransport = UrlConnectionTransport(),
) {
    fun forkInit(): ForkInit {
        val o = call("GET", "/auth/v4/sessions/forks", session = null)
        return ForkInit(userCode = o.optString("UserCode"), selector = o.optString("Selector")).also {
            if (it.userCode.isBlank() || it.selector.isBlank()) throw IOException("Proton started no sign-in")
        }
    }

    /** The fork once somebody has approved it; null while nobody has (Proton answers 422). */
    fun forkStatus(selector: String): ForkApproved? {
        val reply = send("GET", "/auth/v4/sessions/forks/" + encode(selector), session = null, body = null)
        if (reply.status == 422) return null
        val o = parse(reply)
        return ForkApproved(
            uid = o.optString("UID"),
            accessToken = o.optString("AccessToken"),
            refreshToken = o.optString("RefreshToken"),
            payload = o.optString("Payload"),
        )
    }

    /**
     * A fresh access token for [session]. A 4xx other than 429 means the
     * refresh token is no good any more, and the account has to sign in again.
     */
    fun refresh(session: ProtonSession): ProtonSession {
        val body = JSONObject()
            .put("ResponseType", "token")
            .put("GrantType", "refresh_token")
            .put("RefreshToken", session.refreshToken)
            .toString().toByteArray()
        val reply = http.execute("POST", baseUrl + "/auth/v4/refresh", headers(session, json = true, includeToken = false), body)
        if (reply.status in 400..499 && reply.status != 429) {
            throw ProtonSignedOut("Proton no longer accepts this sign-in (${reply.status})")
        }
        val o = parse(reply)
        val access = o.optString("AccessToken")
        if (access.isBlank()) throw IOException("Proton's refresh gave no access token")
        return session.withTokens(
            uid = o.optString("UID").ifBlank { session.uid },
            accessToken = access,
            refreshToken = o.optString("RefreshToken").ifBlank { session.refreshToken },
        )
    }

    fun user(session: ProtonSession): ProtonUser {
        val u = call("GET", "/core/v4/users", session).getJSONObject("User")
        return ProtonUser(
            id = u.optString("ID"),
            name = u.optString("Name"),
            email = u.optString("Email"),
            usedSpace = u.optLong("UsedSpace"),
            maxSpace = u.optLong("MaxSpace"),
            keys = u.optJSONArray("Keys").objects().map {
                ArmoredKey(
                    id = it.optString("ID"),
                    privateKey = it.optString("PrivateKey"),
                    primary = it.optInt("Primary") == 1,
                    active = it.optInt("Active", 1) == 1,
                )
            },
        )
    }

    fun addresses(session: ProtonSession): List<ProtonAddress> =
        call("GET", "/core/v4/addresses", session).optJSONArray("Addresses").objects().map { a ->
            ProtonAddress(
                id = a.optString("ID"),
                email = a.optString("Email"),
                order = a.optInt("Order"),
                status = a.optInt("Status", 1),
                keys = a.optJSONArray("Keys").objects().map {
                    AddressKey(
                        id = it.optString("ID"),
                        privateKey = it.optString("PrivateKey"),
                        token = it.optString("Token").takeUnless { t -> t == "null" }.orEmpty(),
                        signature = it.optString("Signature").takeUnless { s -> s == "null" }.orEmpty(),
                        primary = it.optInt("Primary") == 1,
                        active = it.optInt("Active", 1) == 1,
                        flags = it.optInt("Flags", 3),
                    )
                },
            )
        }.sortedBy { it.order }

    /** Armored public keys for an address, for the SDK's PublicAddressResolver. */
    fun publicKeys(session: ProtonSession, email: String): List<String> {
        val o = call("GET", "/core/v4/keys/all?Email=" + encode(email), session)
        val address = o.optJSONObject("Address") ?: return emptyList()
        return address.optJSONArray("Keys").objects().map { it.optString("PublicKey") }.filter { it.isNotBlank() }
    }

    /** The headers every request to Proton carries, for the SDK's own requests too. */
    fun headers(session: ProtonSession?, json: Boolean = false, includeToken: Boolean = true): Map<String, String> = buildMap {
        put("x-pm-appversion", appVersion)
        put("Accept", "application/vnd.protonmail.v1+json")
        if (json) put("Content-Type", "application/json")
        if (session != null) {
            put("x-pm-uid", session.uid)
            if (includeToken) put("Authorization", "Bearer ${session.accessToken}")
        }
    }

    private fun call(method: String, path: String, session: ProtonSession?): JSONObject =
        parse(send(method, path, session, body = null))

    private fun send(method: String, path: String, session: ProtonSession?, body: ByteArray?): HttpReply {
        val reply = http.execute(method, baseUrl + path, headers(session, json = body != null), body)
        if (reply.status == 401 && session != null) throw ProtonUnauthorized()
        return reply
    }

    private fun parse(reply: HttpReply): JSONObject {
        val o = runCatching { JSONObject(reply.text) }.getOrNull()
        if (reply.status in 200..299 && o != null) return o
        val code = o?.optInt("Code") ?: 0
        val msg = o?.optString("Error")?.takeIf { it.isNotBlank() } ?: "HTTP ${reply.status}"
        throw ProtonRefused(reply.status, code, msg)
    }

    companion object {
        const val DEFAULT_BASE_URL = "https://drive-api.proton.me"

        /**
         * [baseUrl] as the Drive SDK takes it: ending in exactly one '/'. The
         * SDK resolves its paths against it as a directory and refuses to
         * create a client from one without the slash ("Base URL must end with
         * a '/'"). The calls in this class are made the other way round, with
         * the slash at the start of each path.
         */
        fun sdkBaseUrl(baseUrl: String = DEFAULT_BASE_URL): String = baseUrl.trimEnd('/') + "/"

        private fun encode(s: String) = java.net.URLEncoder.encode(s, "UTF-8").replace("+", "%20")
    }
}

private fun JSONArray?.objects(): List<JSONObject> =
    if (this == null) emptyList() else (0 until length()).mapNotNull { optJSONObject(it) }

/**
 * Runs calls with a session, refreshing it when Proton turns its access token
 * down, at most once per rejected token however many calls notice at once.
 */
class SessionKeeper(
    private val book: SessionBook,
    private val refresh: (ProtonSession) -> ProtonSession,
) {
    private val locks = HashMap<String, Any>()

    private fun lockFor(uid: String): Any = synchronized(locks) { locks.getOrPut(uid) { Any() } }

    /**
     * [call] with the session [incoming] stands for, and once more with a
     * refreshed one if it throws [ProtonUnauthorized]. Returns the result and
     * the session it ended up using, which is the one to hand back to the vault.
     */
    fun <T> run(incoming: ProtonSession, call: (ProtonSession) -> T): Pair<T, ProtonSession> {
        val first = book.resolve(incoming)
        try {
            return call(first) to first
        } catch (_: ProtonUnauthorized) {
            val fresh = refreshed(first)
            return try {
                call(fresh) to fresh
            } catch (_: ProtonUnauthorized) {
                throw ProtonSignedOut("Proton turned down a session it had just refreshed")
            }
        }
    }

    /** A session newer than [rejected]: another call's refresh if there was one, else ours. */
    fun refreshed(rejected: ProtonSession): ProtonSession = synchronized(lockFor(rejected.uid)) {
        val current = book.current(rejected.uid)
        if (current != null && current.accessToken != rejected.accessToken) return current
        val fresh = refresh(rejected)
        book.refreshed(rejected, fresh)
        fresh
    }
}
