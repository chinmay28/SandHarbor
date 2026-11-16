package com.sandvault.app.core.proton

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.net.URLDecoder
import java.util.Base64

class ForkSignInTest {
    private val key = ByteArray(32) { it.toByte() }

    @Test
    fun theLinkIsTheOneProtonsCliBuilds() {
        val link = ForkSignIn.link("ABCD-1234", key)
        assertTrue(link.startsWith("https://account.proton.me/desktop/login?app=drive&pv=3#payload="))
        val payload = URLDecoder.decode(link.substringAfter("#payload="), "UTF-8")
        assertEquals("0:ABCD-1234:${Base64.getEncoder().encodeToString(key)}:external-drive", payload)
        // Nothing in the fragment that a browser would take apart.
        assertTrue(link.substringAfter("#payload=").none { it == '+' || it == '/' || it == ':' })
    }

    @Test
    fun readsTheKeyPasswordProtonsPageSealed() {
        val sealed = ForkSignIn.seal(key, "the-key-password")
        assertEquals("the-key-password", ForkSignIn.keyPassword(key, sealed).getOrThrow())
    }

    @Test
    fun aPayloadForAnotherKeyIsRefused() {
        val sealed = ForkSignIn.seal(ByteArray(32) { 7 }, "x")
        assertTrue(ForkSignIn.keyPassword(key, sealed).isFailure)
        assertTrue(ForkSignIn.keyPassword(key, "AAAA").isFailure)
    }

    @Test
    fun theAppSaysWhatItIs() {
        assertEquals("external-drive-sand_vault@2026.11.247-stable", ProtonAppVersion.of("v2026.11.247"))
        assertEquals("external-drive-sand_vault@2026.11.0-alpha", ProtonAppVersion.of("v2026.11.0"))
        assertEquals("external-drive-sand_vault@0.0.0-alpha", ProtonAppVersion.of("dev"))
    }
}
