package com.sandvault.app.core.proton

import java.security.SecureRandom
import java.util.Base64
import java.util.concurrent.TimeUnit
import java.util.concurrent.locks.ReentrantLock
import kotlin.concurrent.withLock

/** Where a sign-in has got to, as the bridge reports it. */
sealed class SignInState {
    data object Pending : SignInState()
    data class Done(val session: ProtonSession) : SignInState()
    data class Failed(val reason: String) : SignInState()
}

/** The two calls a sign-in makes to Proton, apart from looking up who signed in. */
interface ForkApi {
    fun forkInit(): ForkInit
    fun forkStatus(selector: String): ForkApproved?
    fun user(session: ProtonSession): ProtonUser
}

/** [ProtonApi] as a [ForkApi]. */
fun ProtonApi.asForkApi(): ForkApi = object : ForkApi {
    override fun forkInit() = this@asForkApi.forkInit()
    override fun forkStatus(selector: String) = this@asForkApi.forkStatus(selector)
    override fun user(session: ProtonSession) = this@asForkApi.user(session)
}

/**
 * Sign-ins under way. Each runs on a thread of its own, polling Proton as its
 * CLI does — a first wait, then every few seconds, for at most ten minutes —
 * and the bridge asks after it with [await], which waits for news rather than
 * answering at once, so the server's own poll is a long one.
 */
class SignInFlows(
    private val api: ForkApi,
    private val pollEveryMs: Long = 5_000,
    private val firstWaitMs: Long = 5_000,
    private val giveUpAfterMs: Long = 10 * 60_000,
    private val clock: () -> Long = System::currentTimeMillis,
    private val random: SecureRandom = SecureRandom(),
    private val startThread: (Runnable) -> Unit = { Thread(it, "proton-signin").apply { isDaemon = true }.start() },
) {
    private class Flow {
        val lock = ReentrantLock()
        val changed = lock.newCondition()
        var state: SignInState = SignInState.Pending
        var finishedAt = 0L
    }

    private val flows = HashMap<String, Flow>()

    data class Started(val id: String, val url: String)

    /** Asks Proton for a fork and returns the link to follow; the polling goes on behind it. */
    fun start(): Started {
        val init = api.forkInit()
        val key = ForkSignIn.newKey(random)
        val id = ByteArray(16).also(random::nextBytes).let { Base64.getUrlEncoder().withoutPadding().encodeToString(it) }
        val flow = Flow()
        synchronized(flows) {
            forgetOld()
            flows[id] = flow
        }
        startThread(Runnable { finish(flow, poll(init, key)) })
        return Started(id, ForkSignIn.link(init.userCode, key))
    }

    /** The flow's state, waiting up to [waitMs] for it to stop being pending. Null for an unknown flow. */
    fun await(id: String, waitMs: Long): SignInState? {
        val flow = synchronized(flows) { flows[id] } ?: return null
        flow.lock.withLock {
            var left = TimeUnit.MILLISECONDS.toNanos(waitMs)
            while (flow.state is SignInState.Pending && left > 0) left = flow.changed.awaitNanos(left)
            return flow.state
        }
    }

    private fun poll(init: ForkInit, key: ByteArray): SignInState = try {
        val started = clock()
        sleep(firstWaitMs)
        var outcome: SignInState? = null
        while (outcome == null) {
            val approved = api.forkStatus(init.selector)
            if (approved != null) {
                outcome = approve(approved, key)
            } else if (clock() - started > giveUpAfterMs) {
                outcome = SignInState.Failed("nobody followed the sign-in link in time — start again")
            } else {
                sleep(pollEveryMs)
            }
        }
        outcome
    } catch (e: InterruptedException) {
        SignInState.Failed("the sign-in was stopped")
    } catch (e: Exception) {
        SignInState.Failed(e.message ?: e.javaClass.simpleName)
    }

    private fun approve(approved: ForkApproved, key: ByteArray): SignInState {
        val password = ForkSignIn.keyPassword(key, approved.payload).getOrElse {
            return SignInState.Failed("Proton's answer could not be read: ${it.message}")
        }
        val bare = ProtonSession(approved.uid, approved.accessToken, approved.refreshToken, password)
        val user = api.user(bare)
        return SignInState.Done(bare.copy(userId = user.id, email = user.email.ifBlank { user.name }))
    }

    private fun finish(flow: Flow, state: SignInState) = flow.lock.withLock {
        flow.state = state
        flow.finishedAt = clock()
        flow.changed.signalAll()
    }

    /** A finished flow is kept a while for the server to collect, then dropped with what it holds. */
    private fun forgetOld() {
        val now = clock()
        flows.entries.removeIf { (_, f) -> f.finishedAt != 0L && now - f.finishedAt > KEEP_FINISHED_MS }
    }

    private fun sleep(ms: Long) {
        if (ms > 0) Thread.sleep(ms)
    }

    private companion object {
        const val KEEP_FINISHED_MS = 15 * 60_000L
    }
}
