package com.sandvault.app.core

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test
import java.io.File

class ServerLaunchTest {
    private val launch = ServerLaunch(
        binary = File("/data/app/lib/arm64/libsand.so"),
        dataDir = File("/data/user/0/com.sandvault.app/files/sand"),
        cacheDir = File("/data/user/0/com.sandvault.app/cache/sand"),
        port = 8123,
        shareOnNetwork = false,
        machineName = "Pixel 9",
        dnsFile = File("/data/user/0/com.sandvault.app/files/dns"),
        timeZone = "Europe/Berlin",
    )

    @Test
    fun loopbackByDefaultWithNoDiscovery() {
        assertEquals(
            listOf(
                "/data/app/lib/arm64/libsand.so", "serve",
                "--vault", "/data/user/0/com.sandvault.app/files/sand/vault.sand",
                "--port", "8123",
                "--bind", "127.0.0.1",
                "--tv-discovery-port", "0",
            ),
            launch.command(),
        )
    }

    @Test
    fun sharingBindsEveryInterfaceAndAnswersDiscovery() {
        val cmd = launch.copy(shareOnNetwork = true, port = 9000).command()
        assertEquals("0.0.0.0", cmd[cmd.indexOf("--bind") + 1])
        assertEquals("8124", cmd[cmd.indexOf("--tv-discovery-port") + 1])
        assertEquals("9000", cmd[cmd.indexOf("--port") + 1])
    }

    @Test
    fun environmentPointsGoAtTheAppsOwnDirectories() {
        val env = launch.environment()
        assertEquals("/data/user/0/com.sandvault.app/files/sand", env["HOME"])
        assertEquals("/data/user/0/com.sandvault.app/cache/sand", env["TMPDIR"])
        assertEquals("/data/user/0/com.sandvault.app/files/dns", env["SAND_DNS_FILE"])
        assertEquals("1.1.1.1,8.8.8.8", env["SAND_DNS"])
        assertEquals("Europe/Berlin", env["SAND_TZ"])
        assertEquals("Pixel 9", env["SAND_INSTANCE"])
    }

    @Test
    fun blankNameAndZoneAreLeftUnset() {
        val env = launch.copy(machineName = " ", timeZone = "").environment()
        assertFalse(env.containsKey("SAND_INSTANCE"))
        assertFalse(env.containsKey("SAND_TZ"))
    }

    @Test
    fun dnsListDropsScopesAndRepeats() {
        assertEquals(
            "# DNS servers of the phone's current network, written by the SAND Vault app.\n" +
                "192.168.1.1\nfe80::1\n2001:db8::53\n",
            DnsList.render(listOf("/192.168.1.1", "fe80::1%wlan0", "192.168.1.1", " 2001:db8::53 ", "")),
        )
    }

    @Test
    fun machineNames() {
        assertEquals("Chinmay's phone", MachineName.of("Chinmay's phone", "Google", "Pixel 9"))
        assertEquals("Google Pixel 9", MachineName.of(null, "Google", "Pixel 9"))
        assertEquals("Samsung SM-S921B", MachineName.of("", "Samsung", "SM-S921B"))
        assertEquals("Google Pixel", MachineName.of(null, "Google", "Google Pixel"))
        assertEquals("phone", MachineName.of(null, null, null))
        assertEquals("a b", MachineName.of("a\n  b\u0007", null, null))
        assertEquals(48, MachineName.of("x".repeat(100), null, null).length)
    }

    @Test
    fun theBridgeToProtonReachesTheServerOnlyWhenThereIsOne() {
        assertFalse(launch.environment().containsKey("SAND_APP_BRIDGE"))
        val env = launch.copy(bridge = com.sandvault.app.core.proton.BridgeEndpoint("http://127.0.0.1:4100", "tok")).environment()
        assertEquals("http://127.0.0.1:4100", env["SAND_APP_BRIDGE"])
        assertEquals("tok", env["SAND_APP_BRIDGE_TOKEN"])
    }
}
