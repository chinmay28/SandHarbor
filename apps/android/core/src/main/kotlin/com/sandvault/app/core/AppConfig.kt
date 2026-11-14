package com.sandvault.app.core

/** How the app reaches a vault. */
enum class AppMode {
    /** A thin client: the vault runs on another machine, and the app shows it. */
    REMOTE,

    /** Full stack: the vault runs on this phone, and the app shows that. */
    ON_PHONE,
}

/**
 * Everything the person chose in Settings.
 *
 * Both modes' settings are kept side by side, so switching back and forth
 * keeps the other mode's address and port rather than starting over.
 */
data class AppConfig(
    /** Null until the first-run screen has been answered. */
    val mode: AppMode? = null,

    /** REMOTE: the server, as typed; [Origin.parse] reads it. */
    val serverAddress: String = "",

    /** ON_PHONE: the port the server on the phone listens on. */
    val localPort: Int = Origin.SAND_DEFAULT_PORT,

    /**
     * ON_PHONE: also serve other devices on this network — a laptop's
     * browser, a SAND TV — rather than this phone alone.
     */
    val shareOnNetwork: Boolean = false,

    /**
     * ON_PHONE: keep the server running when the app is closed, so uploads
     * and checks carry on and other devices can still reach it.
     */
    val keepRunning: Boolean = false,

    /**
     * ON_PHONE: the name this phone opens the vault under, which the other
     * machines list it as. Blank means the phone's own name.
     */
    val machineName: String = "",
) {
    val configured: Boolean get() = mode != null

    /** The origin the web view opens, or a reason it has none. */
    fun origin(): Result<Origin> = when (mode) {
        null -> Result.failure(IllegalStateException("Choose how to reach a vault first."))
        AppMode.REMOTE -> Origin.parse(serverAddress)
        AppMode.ON_PHONE -> if (localPort in 1024..65535) {
            Result.success(Origin.loopback(localPort))
        } else {
            Result.failure(IllegalArgumentException("The port must be between 1024 and 65535."))
        }
    }

    /**
     * Whether going from this configuration to [next] needs the server on
     * the phone restarted: anything it was started with has changed.
     */
    fun serverChanged(next: AppConfig): Boolean =
        mode != next.mode ||
            (next.mode == AppMode.ON_PHONE &&
                (localPort != next.localPort || shareOnNetwork != next.shareOnNetwork || machineName != next.machineName))
}
