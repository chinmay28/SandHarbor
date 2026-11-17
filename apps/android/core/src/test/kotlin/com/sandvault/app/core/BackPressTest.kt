package com.sandvault.app.core

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class BackPressTest {
    @Test
    fun `the page saying true takes the press`() {
        assertTrue(BackPress.taken("true"))
        assertTrue(BackPress.taken(" true\n"))
    }

    @Test
    fun `anything else leaves it to the app`() {
        for (r in listOf(null, "", "null", "false", "\"true\"", "1", "undefined")) {
            assertFalse("result $r", BackPress.taken(r))
        }
    }

    @Test
    fun `the script asks the bridge and never throws on a page without it`() {
        // Checked as text: no JavaScript engine runs in these tests.
        assertTrue(BackPress.SCRIPT.contains("window.sandApp"))
        assertTrue(BackPress.SCRIPT.contains("typeof a.back==='function'"))
        assertTrue(BackPress.SCRIPT.startsWith("(function(){") && BackPress.SCRIPT.endsWith("})()"))
    }
}
