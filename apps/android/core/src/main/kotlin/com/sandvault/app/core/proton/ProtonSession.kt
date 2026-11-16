package com.sandvault.app.core.proton

import org.json.JSONObject
import java.util.Base64

/**
 * A signed-in Proton session, as the vault keeps it (the `session` secret of a
 * `protonapp` account) and as it travels to the app with every request.
 *
 * [keyPassword] is what Proton's sign-in page hands over: it unlocks the
 * account's keys. It is the same secret Proton's own CLI keeps in its session
 * file, and it is why the whole thing lives encrypted in the vault rather than
 * on the phone's disk. See docs/design/proton-android.md.
 */
data class ProtonSession(
    val uid: String,
    val accessToken: String,
    val refreshToken: String,
    val keyPassword: String,
    val userId: String = "",
    val email: String = "",
) {
    init {
        require(uid.isNotBlank()) { "a Proton session needs a UID" }
        require(accessToken.isNotBlank()) { "a Proton session needs an access token" }
        require(keyPassword.isNotBlank()) { "a Proton session needs the key password" }
    }

    fun toJson(): String = JSONObject()
        .put("v", VERSION)
        .put("uid", uid)
        .put("access_token", accessToken)
        .put("refresh_token", refreshToken)
        .put("key_password", keyPassword)
        .put("user_id", userId)
        .put("email", email)
        .toString()

    /** The form it crosses HTTP in, in the X-Sand-Proton-Session header. */
    fun toHeader(): String = Base64.getEncoder().encodeToString(toJson().toByteArray(Charsets.UTF_8))

    fun withTokens(uid: String, accessToken: String, refreshToken: String) =
        copy(uid = uid, accessToken = accessToken, refreshToken = refreshToken)

    // The tokens and the key password must never reach a log.
    override fun toString(): String = "ProtonSession(uid=${uid.take(6)}…, email=$email)"

    companion object {
        const val VERSION = 1

        fun fromJson(text: String): Result<ProtonSession> = runCatching {
            val o = JSONObject(text)
            val v = o.optInt("v", 0)
            require(v == VERSION) { "a Proton session of version $v, which this app does not read" }
            ProtonSession(
                uid = o.optString("uid"),
                accessToken = o.optString("access_token"),
                refreshToken = o.optString("refresh_token"),
                keyPassword = o.optString("key_password"),
                userId = o.optString("user_id"),
                email = o.optString("email"),
            )
        }

        fun fromHeader(header: String?): Result<ProtonSession> = runCatching {
            require(!header.isNullOrBlank()) { "no Proton session sent" }
            String(Base64.getDecoder().decode(header.trim()), Charsets.UTF_8)
        }.mapCatching { fromJson(it).getOrThrow() }
    }
}

/**
 * The newest session the app knows of for each Proton session UID.
 *
 * The vault sends a session with every request, but the vault learns of a
 * refresh only when an answer carrying it gets back — and several requests are
 * in flight at once. Proton's refresh token is single-use, so a request that
 * set out with the session from before a refresh must not try to use (or
 * refresh) it again: it would sign the account out. So the app remembers which
 * refresh tokens it has spent, and a request arriving with one of those is
 * given the session that replaced it.
 *
 * A session the app has not seen spent is taken as it comes. That is how a
 * fresh sign-in, or a session the vault restored from a recovery kit on
 * another phone, replaces whatever was here.
 */
class SessionBook {
    private val latest = HashMap<String, ProtonSession>()
    private val spent = HashMap<String, MutableSet<String>>()

    /** The session to use for a request that arrived with [incoming]. */
    @Synchronized
    fun resolve(incoming: ProtonSession): ProtonSession {
        val known = latest[incoming.uid]
        if (known != null && incoming.refreshToken in spent[incoming.uid].orEmpty()) return known
        latest[incoming.uid] = incoming
        return incoming
    }

    /** [before] was refreshed into [after]: [before]'s refresh token is spent. */
    @Synchronized
    fun refreshed(before: ProtonSession, after: ProtonSession) {
        spent.getOrPut(before.uid) { HashSet() } += before.refreshToken
        if (after.uid != before.uid) spent.getOrPut(after.uid) { HashSet() } += before.refreshToken
        latest[after.uid] = after
    }

    @Synchronized
    fun current(uid: String): ProtonSession? = latest[uid]
}
