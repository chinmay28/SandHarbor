package com.sandvault.app.core.proton

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

class ProtonApiTest {
    private class Recorded(val method: String, val url: String, val headers: Map<String, String>, val body: String?)

    private val seen = mutableListOf<Recorded>()

    private fun api(answer: (Recorded) -> HttpReply) = ProtonApi(
        appVersion = "external-drive-sand_vault@2026.11.1-stable",
        baseUrl = "https://proton.test",
        http = { m, u, h, b -> Recorded(m, u, h, b?.let { String(it) }).also { seen += it }.let(answer) },
    )

    private fun ok(json: String) = HttpReply(200, json.toByteArray())

    private val session = ProtonSession("uid", "access", "refresh", "pw")

    @Test
    fun startsAndFollowsAFork() {
        val a = api {
            when {
                it.url.endsWith("/auth/v4/sessions/forks") -> ok("""{"Code":1000,"UserCode":"UC","Selector":"SEL"}""")
                it.url.endsWith("/forks/SEL") -> HttpReply(422, """{"Code":9001}""".toByteArray())
                else -> error(it.url)
            }
        }
        assertEquals(ForkInit("UC", "SEL"), a.forkInit())
        assertNull("422 is Proton saying nobody has approved yet", a.forkStatus("SEL"))
        assertEquals("external-drive-sand_vault@2026.11.1-stable", seen[0].headers["x-pm-appversion"])
        assertNull("a fork is asked for before there is a session", seen[0].headers["Authorization"])
    }

    @Test
    fun refreshesWithTheRefreshToken() {
        val a = api { ok("""{"Code":1000,"UID":"uid","AccessToken":"access2","RefreshToken":"refresh2"}""") }
        val fresh = a.refresh(session)
        assertEquals("access2", fresh.accessToken)
        assertEquals("refresh2", fresh.refreshToken)
        assertEquals("pw", fresh.keyPassword)
        val body = JSONObject(seen.single().body!!)
        assertEquals("refresh", body.getString("RefreshToken"))
        assertEquals("refresh_token", body.getString("GrantType"))
        assertEquals("uid", seen.single().headers["x-pm-uid"])
    }

    @Test
    fun aRefusedRefreshIsASignOutButARateLimitIsNot() {
        try {
            api { HttpReply(400, """{"Code":10013,"Error":"Invalid refresh token"}""".toByteArray()) }.refresh(session)
            fail("expected a sign-out")
        } catch (_: ProtonSignedOut) {
        }
        try {
            api { HttpReply(429, """{"Code":2028,"Error":"Too many requests"}""".toByteArray()) }.refresh(session)
            fail("expected a refusal")
        } catch (e: ProtonRefused) {
            assertEquals(429, e.status)
        }
    }

    @Test
    fun readsTheUserAndTheAddresses() {
        val a = api {
            when {
                it.url.endsWith("/core/v4/users") -> ok(
                    """{"Code":1000,"User":{"ID":"U1","Name":"someone","Email":"someone@proton.me","UsedSpace":10,"MaxSpace":500,
                       "Keys":[{"ID":"K1","PrivateKey":"armored","Primary":1}]}}""",
                )
                it.url.endsWith("/core/v4/addresses") -> ok(
                    """{"Code":1000,"Addresses":[
                       {"ID":"A2","Email":"alias@proton.me","Order":2,"Status":1,"Keys":[]},
                       {"ID":"A1","Email":"someone@proton.me","Order":1,"Status":1,
                        "Keys":[{"ID":"AK1","PrivateKey":"ak","Token":"tok","Signature":"sig","Primary":1,"Flags":3}]}]}""",
                )
                else -> error(it.url)
            }
        }
        val user = a.user(session)
        assertEquals("someone@proton.me", user.email)
        assertEquals(500, user.maxSpace)
        assertTrue(user.keys.single().primary)
        val addresses = a.addresses(session)
        assertEquals(listOf("A1", "A2"), addresses.map { it.id })
        assertEquals("tok", addresses[0].keys.single().token)
        assertEquals("Bearer access", seen[0].headers["Authorization"])
    }

    @Test
    fun aRejectedAccessTokenIsRefreshedOnceAndRetried() {
        val book = SessionBook()
        val refreshes = AtomicInteger()
        val keeper = SessionKeeper(book) { s ->
            refreshes.incrementAndGet()
            s.withTokens(s.uid, "access2", "refresh2")
        }
        val (answer, used) = keeper.run(session) { s ->
            if (s.accessToken == "access") throw ProtonUnauthorized()
            "worked with ${s.accessToken}"
        }
        assertEquals("worked with access2", answer)
        assertEquals("refresh2", used.refreshToken)
        assertEquals(1, refreshes.get())
        // A request still carrying the spent session is given the new one, with no second refresh.
        val (_, again) = keeper.run(session) { it.accessToken }
        assertEquals("access2", again.accessToken)
        assertEquals(1, refreshes.get())
    }

    @Test
    fun callsThatAllNoticeAtOnceShareOneRefresh() {
        val book = SessionBook()
        val refreshes = AtomicInteger()
        val keeper = SessionKeeper(book) { s ->
            refreshes.incrementAndGet()
            Thread.sleep(50)
            s.withTokens(s.uid, "access2", "refresh2")
        }
        book.resolve(session)
        val pool = Executors.newFixedThreadPool(8)
        val go = CountDownLatch(1)
        repeat(8) {
            pool.execute {
                go.await()
                keeper.run(session) { s -> if (s.accessToken == "access") throw ProtonUnauthorized() else Unit }
            }
        }
        go.countDown()
        pool.shutdown()
        assertTrue(pool.awaitTermination(5, TimeUnit.SECONDS))
        assertEquals("a single-use refresh token may be spent once", 1, refreshes.get())
    }

    @Test
    fun aSessionRejectedEvenAfterARefreshIsSignedOut() {
        val keeper = SessionKeeper(SessionBook()) { it.withTokens(it.uid, "access2", "refresh2") }
        try {
            keeper.run(session) { throw ProtonUnauthorized() }
            fail("expected a sign-out")
        } catch (_: ProtonSignedOut) {
        }
    }

    /** The Drive SDK will not create a client from a base URL without its trailing slash. */
    @Test
    fun theSdkIsGivenABaseUrlEndingInOneSlash() {
        assertEquals("https://drive-api.proton.me/", ProtonApi.sdkBaseUrl())
        assertEquals("https://proton.test/", ProtonApi.sdkBaseUrl("https://proton.test"))
        assertEquals("https://proton.test/", ProtonApi.sdkBaseUrl("https://proton.test/"))
        assertEquals("https://proton.test/", ProtonApi.sdkBaseUrl("https://proton.test//"))
    }
}
