package com.sandvault.app.proton

import android.util.Log
import com.sandvault.app.BuildConfig
import com.sandvault.app.core.proton.BridgeEndpoint
import com.sandvault.app.core.proton.ProtonApi
import com.sandvault.app.core.proton.ProtonAppVersion
import com.sandvault.app.core.proton.ProtonBridge
import com.sandvault.app.core.proton.SignInFlows
import com.sandvault.app.core.proton.asForkApi

/**
 * The one bridge to Proton per app process, started the first time the vault
 * on the phone is, and kept for as long as the process lives — the server it
 * serves is that process's child. Its address and token go into the
 * server's environment (ServerLaunch.bridge).
 *
 * If it cannot start, the vault still does: Proton Drive (this phone) is then
 * not offered, which is the same as running on a machine without the app.
 */
object ProtonBridgeHost {
    private const val TAG = "SandProton"

    @Volatile
    private var endpoint: BridgeEndpoint? = null

    @Synchronized
    fun ensure(): BridgeEndpoint? {
        endpoint?.let { return it }
        return runCatching {
            val api = ProtonApi(appVersion = ProtonAppVersion.of(BuildConfig.VERSION_NAME))
            ProtonBridge(SdkProtonStore(api), SignInFlows(api.asForkApi())).begin()
        }.onFailure { Log.w(TAG, "the bridge to Proton did not start", it) }
            .getOrNull()
            .also { endpoint = it }
    }
}
