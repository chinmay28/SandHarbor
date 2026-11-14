package com.sandvault.app.core

import java.io.File

/**
 * How the vault on the phone is started: the command line and environment
 * for `sand serve`, worked out from the settings and the app's directories.
 *
 * Kept apart from starting it so that what the server is told can be read,
 * and tested, without a phone.
 */
data class ServerLaunch(
    /** The sand binary, packaged as libsand.so so Android installs it executable. */
    val binary: File,

    /** Private to the app: the vault file, machine.json and everything else sand keeps. */
    val dataDir: File,

    /** Private scratch space, which Android may clear. */
    val cacheDir: File,

    val port: Int,
    val shareOnNetwork: Boolean,

    /** What other machines list this phone as. */
    val machineName: String,

    /** The file [DnsList] writes and the server re-reads; see internal/hostenv. */
    val dnsFile: File,

    /** An IANA zone name, e.g. "Europe/Berlin". */
    val timeZone: String,
) {
    val vaultFile: File get() = File(dataDir, "vault.sand")

    fun command(): List<String> = listOf(
        binary.absolutePath,
        "serve",
        "--vault", vaultFile.absolutePath,
        "--port", port.toString(),
        // Loopback unless asked: then only this phone's apps can reach it,
        // and the browser's cookie is the only key that matters.
        "--bind", if (shareOnNetwork) "0.0.0.0" else "127.0.0.1",
        // Answering a TV's broadcast only makes sense on a server it can reach.
        "--tv-discovery-port", if (shareOnNetwork) DiscoveryProtocol.PORT.toString() else "0",
    )

    fun environment(): Map<String, String> = buildMap {
        // Go reads these for the home directory and temporary files; on
        // Android it would otherwise pick /sdcard and /data/local/tmp, which
        // the app cannot write.
        put("HOME", dataDir.absolutePath)
        put("TMPDIR", cacheDir.absolutePath)
        put("XDG_CONFIG_HOME", File(dataDir, ".config").absolutePath)
        put("XDG_CACHE_HOME", cacheDir.absolutePath)
        put("SAND_DNS_FILE", dnsFile.absolutePath)
        // Used only while the phone reports no servers of its own: some VPNs
        // and carriers do not say.
        put("SAND_DNS", DnsList.FALLBACK.joinToString(","))
        if (timeZone.isNotBlank()) put("SAND_TZ", timeZone)
        // Two phones would both be "localhost" otherwise — the hostname
        // Android gives every app — and two machines of one name are refused.
        if (machineName.isNotBlank()) put("SAND_INSTANCE", machineName.trim())
    }
}

/** The DNS server file the vault on the phone reads (internal/hostenv). */
object DnsList {
    /** Asked only when the phone names no DNS servers at all. */
    val FALLBACK = listOf("1.1.1.1", "8.8.8.8")

    /**
     * The file's contents for these servers: one address per line, IPv6
     * scopes dropped (the server cannot use them), duplicates once.
     */
    fun render(servers: List<String>): String {
        val lines = servers
            .map { it.trim().removePrefix("/").substringBefore('%') }
            .filter { it.isNotEmpty() }
            .distinct()
        return buildString {
            append("# DNS servers of the phone's current network, written by the SAND Vault app.\n")
            for (line in lines) append(line).append('\n')
        }
    }
}

/** What a phone gets called when the person has not named it. */
object MachineName {
    /**
     * A name from the phone's own: the name the person gave the device, else
     * its maker and model ("Pixel 9"), else "phone". Reduced to what reads
     * well in a list and travels in an environment variable.
     */
    fun of(deviceName: String?, manufacturer: String?, model: String?): String {
        val chosen = deviceName?.takeIf { it.isNotBlank() }
            ?: listOfNotNull(manufacturer?.takeIf { it.isNotBlank() }, model?.takeIf { it.isNotBlank() })
                .let { parts ->
                    // "Google Pixel 9" reads oddly when the model already says it.
                    if (parts.size == 2 && parts[1].startsWith(parts[0], ignoreCase = true)) parts[1] else parts.joinToString(" ")
                }
                .takeIf { it.isNotBlank() }
            ?: "phone"
        val cleaned = chosen.filter { !it.isISOControl() }.trim().replace(Regex("\\s+"), " ")
        return cleaned.take(48).ifBlank { "phone" }
    }
}
