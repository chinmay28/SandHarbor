package com.sandvault.app.core.proton

import org.json.JSONArray
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.net.HttpURLConnection
import java.net.URL
import java.util.concurrent.ConcurrentHashMap

class ProtonBridgeTest {
    private val session = ProtonSession("uid", "access", "refresh", "pw", email = "someone@proton.me")

    /** A folder of parts in a map; refreshes the session on the first call when asked to. */
    private class MapStore : ProtonStore {
        val parts = ConcurrentHashMap<String, ByteArray>()
        val folders = mutableListOf<String>()
        var refreshTo: ProtonSession? = null
        var signedOut = false

        private fun touch(call: AccountCall) {
            synchronized(folders) { folders += call.folder }
            if (signedOut) throw BridgeFailure.SignedOut("revoked")
            refreshTo?.let { call.refreshed = it; refreshTo = null }
        }

        override fun ping(call: AccountCall) = touch(call)
        override fun email(call: AccountCall) = touch(call).let { call.session.email }
        override fun usage(call: AccountCall) = touch(call).let { 5L to 100L }
        override fun list(call: AccountCall, prefix: String) =
            touch(call).let { parts.filterKeys { it.startsWith(prefix) }.map { StoredPart(it.key, it.value.size.toLong()) } }
        override fun get(call: AccountCall, key: String) = touch(call).let { parts[key] ?: throw BridgeFailure.NotFound() }
        override fun stat(call: AccountCall, key: String) = get(call, key).let { StoredPart(key, it.size.toLong()) }
        override fun put(call: AccountCall, key: String, data: ByteArray) {
            touch(call); parts[key] = data
        }
        override fun delete(call: AccountCall, key: String) {
            touch(call); parts.remove(key) ?: throw BridgeFailure.NotFound()
        }
    }

    private class FakeForks : ForkApi {
        @Volatile var approved: ForkApproved? = null
        override fun forkInit() = ForkInit("UC", "SEL")
        override fun forkStatus(selector: String) = approved
        override fun user(session: ProtonSession) = ProtonUser("U1", "someone", "someone@proton.me", 0, 0, emptyList())
    }

    private val store = MapStore()
    private val forks = FakeForks()
    private lateinit var bridge: ProtonBridge
    private lateinit var endpoint: BridgeEndpoint

    @Before
    fun up() {
        bridge = ProtonBridge(store, SignInFlows(forks, pollEveryMs = 10, firstWaitMs = 0), token = "the-token", longPollMs = 300)
        endpoint = bridge.begin()
    }

    @After
    fun down() = bridge.stop()

    private class Answer(val status: Int, val body: ByteArray, val session: String?) {
        val text get() = String(body)
    }

    private fun ask(
        method: String,
        path: String,
        body: ByteArray? = null,
        token: String = "the-token",
        withSession: Boolean = true,
    ): Answer {
        val conn = URL(endpoint.url + path).openConnection() as HttpURLConnection
        conn.requestMethod = method
        conn.setRequestProperty("Authorization", "Bearer $token")
        if (withSession) {
            conn.setRequestProperty(ProtonBridge.SESSION_HEADER, session.toHeader())
            conn.setRequestProperty(ProtonBridge.FOLDER_HEADER, "/my-files/sand")
        }
        if (body != null) {
            conn.doOutput = true
            conn.setFixedLengthStreamingMode(body.size)
            conn.outputStream.use { it.write(body) }
        }
        val status = conn.responseCode
        val data = (if (status >= 400) conn.errorStream else conn.inputStream)?.use { it.readBytes() } ?: ByteArray(0)
        return Answer(status, data, conn.getHeaderField(ProtonBridge.SESSION_HEADER))
    }

    @Test
    fun listensOnLoopbackOnly() {
        assertTrue(endpoint.url.startsWith("http://127.0.0.1:"))
        assertEquals(mapOf("SAND_APP_BRIDGE" to endpoint.url, "SAND_APP_BRIDGE_TOKEN" to "the-token"), endpoint.environment())
    }

    @Test
    fun turnsAwayAnythingWithoutTheToken() {
        assertEquals(401, ask("GET", "/proton/v1/ping", token = "guess").status)
        assertTrue(store.folders.isEmpty())
    }

    @Test
    fun storesFetchesListsAndDeletesParts() {
        val part = ByteArray(300_000) { (it % 251).toByte() }
        assertEquals(204, ask("PUT", "/proton/v1/objects/abc-p1.sand", part).status)
        assertArrayEquals(part, ask("GET", "/proton/v1/objects/abc-p1.sand").body)
        assertEquals(300_000, JSONObject(ask("GET", "/proton/v1/objects/abc-p1.sand?stat=1").text).getLong("size"))
        val list = JSONArray(ask("GET", "/proton/v1/objects?prefix=abc").text)
        assertEquals("abc-p1.sand", list.getJSONObject(0).getString("key"))
        assertEquals(204, ask("DELETE", "/proton/v1/objects/abc-p1.sand").status)
        val gone = ask("GET", "/proton/v1/objects/abc-p1.sand")
        assertEquals(404, gone.status)
        assertEquals("not_found", JSONObject(gone.text).getString("code"))
        assertTrue(store.folders.all { it == "/my-files/sand" })
    }

    @Test
    fun answersPingQuotaAndAccount() {
        assertEquals(204, ask("GET", "/proton/v1/ping").status)
        assertEquals(100, JSONObject(ask("GET", "/proton/v1/usage").text).getLong("total"))
        assertEquals("someone@proton.me", JSONObject(ask("GET", "/proton/v1/account").text).getString("email"))
    }

    @Test
    fun handsARefreshedSessionBack() {
        val fresh = session.withTokens("uid", "access2", "refresh2")
        store.refreshTo = fresh
        val answer = ask("GET", "/proton/v1/ping")
        assertEquals(fresh, ProtonSession.fromHeader(answer.session).getOrThrow())
        assertEquals(null, ask("GET", "/proton/v1/ping").session)
    }

    @Test
    fun saysWhenProtonSignedTheAccountOut() {
        store.signedOut = true
        val answer = ask("GET", "/proton/v1/ping")
        assertEquals(401, answer.status)
        assertEquals("signed_out", JSONObject(answer.text).getString("code"))
    }

    @Test
    fun refusesRequestsItCannotPlace() {
        assertEquals(400, ask("GET", "/proton/v1/ping", withSession = false).status)
        assertEquals(400, ask("GET", "/proton/v1/nowhere").status)
    }

    @Test
    fun signsInByLink() {
        val start = JSONObject(ask("POST", "/proton/v1/signin", "{}".toByteArray(), withSession = false).text)
        val url = start.getString("url")
        assertTrue(url.startsWith("https://account.proton.me/desktop/login"))
        val id = start.getString("id")
        assertEquals("pending", JSONObject(ask("GET", "/proton/v1/signin/$id", withSession = false).text).getString("state"))

        // Somebody follows the link: Proton's page seals the key password under the link's key.
        val key = java.util.Base64.getDecoder().decode(
            java.net.URLDecoder.decode(url.substringAfter("#payload="), "UTF-8").split(":")[2],
        )
        forks.approved = ForkApproved("uid9", "a9", "r9", ForkSignIn.seal(key, "the-key-password"))

        var state = JSONObject()
        repeat(20) {
            state = JSONObject(ask("GET", "/proton/v1/signin/$id", withSession = false).text)
            if (state.getString("state") != "pending") return@repeat
        }
        assertEquals("done", state.getString("state"))
        assertEquals("someone@proton.me", state.getString("email"))
        val signedIn = ProtonSession.fromJson(state.getString("session")).getOrThrow()
        assertEquals("the-key-password", signedIn.keyPassword)
        assertEquals("U1", signedIn.userId)
        assertEquals(404, ask("GET", "/proton/v1/signin/nobody", withSession = false).status)
    }
}
