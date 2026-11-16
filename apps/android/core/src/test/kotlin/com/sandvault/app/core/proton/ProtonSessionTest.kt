package com.sandvault.app.core.proton

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

class ProtonSessionTest {
    private val session = ProtonSession("uid-1", "access-1", "refresh-1", "key-pass", "user-1", "a@proton.me")

    @Test
    fun roundTripsThroughJsonAndTheHeader() {
        assertEquals(session, ProtonSession.fromJson(session.toJson()).getOrThrow())
        assertEquals(session, ProtonSession.fromHeader(session.toHeader()).getOrThrow())
    }

    @Test
    fun refusesWhatItCannotUse() {
        assertTrue(ProtonSession.fromHeader(null).isFailure)
        assertTrue(ProtonSession.fromHeader("not base64 !").isFailure)
        assertTrue(ProtonSession.fromJson("""{"v":2,"uid":"u","access_token":"a","key_password":"k"}""").isFailure)
        assertTrue(ProtonSession.fromJson("""{"v":1,"uid":"u","access_token":"a"}""").isFailure)
    }

    @Test
    fun neverPrintsItsSecrets() {
        val printed = session.toString()
        assertFalse(printed.contains("access-1"))
        assertFalse(printed.contains("refresh-1"))
        assertFalse(printed.contains("key-pass"))
    }

    @Test
    fun aRequestWithASpentSessionGetsItsReplacement() {
        val book = SessionBook()
        assertSame(session, book.resolve(session))
        val fresh = session.withTokens("uid-1", "access-2", "refresh-2")
        book.refreshed(session, fresh)
        // The vault has not heard of the refresh yet and sends the old one.
        assertEquals(fresh, book.resolve(session))
    }

    @Test
    fun aSessionNotSeenSpentReplacesWhatWasKnown() {
        val book = SessionBook()
        book.resolve(session)
        val signedInAgain = session.withTokens("uid-1", "access-9", "refresh-9")
        assertEquals(signedInAgain, book.resolve(signedInAgain))
        assertEquals(signedInAgain, book.current("uid-1"))
    }
}
