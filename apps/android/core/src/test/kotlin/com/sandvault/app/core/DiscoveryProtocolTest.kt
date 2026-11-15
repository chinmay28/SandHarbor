package com.sandvault.app.core

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class DiscoveryProtocolTest {
    @Test
    fun readsAnAnnouncement() {
        val f = DiscoveryProtocol.parse("""{"service":"sand-vault","name":"raspberrypi","port":8123,"version":"v2026.11.240"}""", "/192.168.1.20")!!
        assertEquals("raspberrypi", f.name)
        assertEquals("http://192.168.1.20:8123", f.origin.toString())
        assertEquals("v2026.11.240", f.version)
    }

    @Test
    fun secureMeansHttpsAndAMissingNameIsTheHost() {
        val f = DiscoveryProtocol.parse("""{"service":"sand-vault","port":443,"secure":true}""", "fd00::20%wlan0")!!
        assertEquals("https://[fd00::20]", f.origin.toString())
        assertEquals("fd00::20", f.name)
    }

    @Test
    fun ignoresAnythingElse() {
        assertNull(DiscoveryProtocol.parse("""{"service":"other","port":8123}""", "10.0.0.1"))
        assertNull(DiscoveryProtocol.parse("""{"service":"sand-vault","port":0}""", "10.0.0.1"))
        assertNull(DiscoveryProtocol.parse("SAND-TV-DISCOVER/1", "10.0.0.1"))
        assertNull(DiscoveryProtocol.parse("""{"service":"sand-vault","port":8123}""", ""))
    }
}
