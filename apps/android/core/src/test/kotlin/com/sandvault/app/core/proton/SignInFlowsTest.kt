package com.sandvault.app.core.proton

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class SignInFlowsTest {
    private class NeverApproved : ForkApi {
        override fun forkInit() = ForkInit("UC", "SEL")
        override fun forkStatus(selector: String): ForkApproved? = null
        override fun user(session: ProtonSession) = error("nobody signed in")
    }

    @Test
    fun givesUpWhenNobodyFollowsTheLink() {
        val flows = SignInFlows(NeverApproved(), pollEveryMs = 5, firstWaitMs = 0, giveUpAfterMs = 50)
        val started = flows.start()
        val state = flows.await(started.id, waitMs = 2_000)
        assertTrue(state is SignInState.Failed)
        assertTrue((state as SignInState.Failed).reason.contains("in time"))
    }

    @Test
    fun aPayloadItCannotReadFailsTheSignIn() {
        val api = object : ForkApi {
            override fun forkInit() = ForkInit("UC", "SEL")
            override fun forkStatus(selector: String) = ForkApproved("u", "a", "r", ForkSignIn.seal(ByteArray(32), "x"))
            override fun user(session: ProtonSession) = error("not reached")
        }
        val flows = SignInFlows(api, pollEveryMs = 5, firstWaitMs = 0)
        val state = flows.await(flows.start().id, waitMs = 2_000)
        assertTrue(state is SignInState.Failed)
    }

    @Test
    fun waitsForNewsRatherThanAnsweringAtOnce() {
        val flows = SignInFlows(NeverApproved(), pollEveryMs = 1_000, firstWaitMs = 0)
        val id = flows.start().id
        val before = System.nanoTime()
        assertEquals(SignInState.Pending, flows.await(id, waitMs = 150))
        assertTrue((System.nanoTime() - before) / 1_000_000 >= 140)
        assertNull(flows.await("unknown", waitMs = 0))
    }
}
