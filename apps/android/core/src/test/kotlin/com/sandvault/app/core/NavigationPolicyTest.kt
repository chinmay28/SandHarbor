package com.sandvault.app.core

import org.junit.Assert.assertEquals
import org.junit.Test

class NavigationPolicyTest {
    private val app = Origin.loopback(8123)

    private fun check(url: String, want: Navigation) = assertEquals(url, want, NavigationPolicy.decide(app, url))

    @Test
    fun theVaultsOwnPagesStayInTheApp() {
        check("http://127.0.0.1:8123/", Navigation.IN_APP)
        check("http://127.0.0.1:8123/api/files/1/content", Navigation.IN_APP)
        check("blob:http://127.0.0.1:8123/0b1c", Navigation.IN_APP)
        check("data:image/png;base64,AAAA", Navigation.IN_APP)
        check("about:blank", Navigation.IN_APP)
    }

    @Test
    fun otherSitesGoToThePhone() {
        check("https://accounts.google.com/o/oauth2/auth?x=1", Navigation.EXTERNAL)
        check("http://127.0.0.1:9999/", Navigation.EXTERNAL)
        check("https://127.0.0.1:8123/", Navigation.EXTERNAL)
        check("mailto:someone@example.com", Navigation.EXTERNAL)
        check("tel:+15555550100", Navigation.EXTERNAL)
        check("vlc://127.0.0.1:8123/api/stream/x", Navigation.EXTERNAL)
    }

    @Test
    fun intentUrlsAreTheirOwnCase() {
        check("intent://127.0.0.1:8123/api/stream/x#Intent;scheme=http;type=video/*;package=org.videolan.vlc;end", Navigation.INTENT)
    }

    @Test
    fun dangerousSchemesAreBlocked() {
        check("javascript:alert(1)", Navigation.BLOCK)
        check("file:///data/data/com.sandvault.app/files/sand/vault.sand", Navigation.BLOCK)
        check("content://com.android.contacts/contacts", Navigation.BLOCK)
        check("about:config", Navigation.BLOCK)
        check("weird", Navigation.BLOCK)
        check("", Navigation.BLOCK)
    }
}
