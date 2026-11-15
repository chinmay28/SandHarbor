package com.sandvault.app

import android.content.Context
import android.content.SharedPreferences
import android.os.Build
import android.provider.Settings
import com.sandvault.app.core.AppConfig
import com.sandvault.app.core.AppMode
import com.sandvault.app.core.MachineName
import com.sandvault.app.core.Origin

/**
 * The settings, kept in the shared preferences the settings screen edits
 * directly — one store, so the screen and the rest of the app never disagree.
 */
class ConfigStore(context: Context) {
    private val app = context.applicationContext
    val prefs: SharedPreferences = app.getSharedPreferences(FILE, Context.MODE_PRIVATE)

    fun load(): AppConfig = AppConfig(
        mode = when (prefs.getString(KEY_MODE, null)) {
            MODE_REMOTE -> AppMode.REMOTE
            MODE_ON_PHONE -> AppMode.ON_PHONE
            else -> null
        },
        serverAddress = prefs.getString(KEY_ADDRESS, "") ?: "",
        localPort = prefs.getString(KEY_PORT, null)?.toIntOrNull() ?: Origin.SAND_DEFAULT_PORT,
        shareOnNetwork = prefs.getBoolean(KEY_SHARE, false),
        keepRunning = prefs.getBoolean(KEY_KEEP, false),
        machineName = prefs.getString(KEY_MACHINE, "") ?: "",
    )

    fun save(c: AppConfig) {
        prefs.edit()
            .putString(KEY_MODE, c.mode?.let(::modeValue))
            .putString(KEY_ADDRESS, c.serverAddress)
            .putString(KEY_PORT, c.localPort.toString())
            .putBoolean(KEY_SHARE, c.shareOnNetwork)
            .putBoolean(KEY_KEEP, c.keepRunning)
            .putString(KEY_MACHINE, c.machineName)
            .apply()
    }

    /** The name the vault opens under: the person's, else the phone's own. */
    fun machineName(c: AppConfig = load()): String =
        c.machineName.trim().ifEmpty { defaultMachineName() }

    fun defaultMachineName(): String = MachineName.of(
        runCatching { Settings.Global.getString(app.contentResolver, Settings.Global.DEVICE_NAME) }.getOrNull(),
        Build.MANUFACTURER,
        Build.MODEL,
    )

    companion object {
        const val FILE = "sand"
        const val KEY_MODE = "mode"
        const val KEY_ADDRESS = "server_address"
        const val KEY_PORT = "local_port"
        const val KEY_SHARE = "share_on_network"
        const val KEY_KEEP = "keep_running"
        const val KEY_MACHINE = "machine_name"
        const val MODE_REMOTE = "remote"
        const val MODE_ON_PHONE = "on_phone"

        fun modeValue(m: AppMode) = if (m == AppMode.REMOTE) MODE_REMOTE else MODE_ON_PHONE
    }
}
