package com.sandvault.tv

import com.sandvault.tv.BlurayStreams.Reading
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Random

class BlurayStreamsTest {

    /** A stream's descriptors as a Blu-ray writes them: the registration, then whatever else. */
    private fun hdmv(type: Int) = byteArrayOf(
        0x05, 0x08, 'H'.code.toByte(), 'D'.code.toByte(), 'M'.code.toByte(), 'V'.code.toByte(),
        0xff.toByte(), type.toByte(), 0x61, 0x3f,
    )

    /** A broadcast stream's: a language, and a registration that is somebody else's. */
    private val broadcast = byteArrayOf(
        0x0a, 0x04, 'e'.code.toByte(), 'n'.code.toByte(), 'g'.code.toByte(), 0x00,
        0x05, 0x04, 'C'.code.toByte(), 'U'.code.toByte(), 'E'.code.toByte(), 'I'.code.toByte(),
    )

    @Test
    fun aBluraysSoundIsReadAsWhatABlurayMeans() {
        // The three DTS kinds: the core is in all of them.
        for (type in listOf(0x82, 0x85, 0x86)) {
            assertEquals("type %#x".format(type), Reading.DTS_CORE, BlurayStreams.reading(type, hdmv(type)))
        }
        // PCM is not video, and nothing here plays it out of a transport stream.
        assertEquals(Reading.IGNORE, BlurayStreams.reading(0x80, hdmv(0x80)))
        // What Media3 already reads correctly is left to it.
        for (type in listOf(0x1b, 0x24, 0x81, 0x83, 0x84, 0x90)) {
            assertEquals("type %#x".format(type), Reading.AS_IS, BlurayStreams.reading(type, hdmv(type)))
        }
    }

    @Test
    fun aStreamThatIsNotABluraysIsLeftAlone() {
        // 0x86 off the air really is splice data, and 0x80 really is video.
        for (type in listOf(0x80, 0x82, 0x86)) {
            assertEquals(Reading.AS_IS, BlurayStreams.reading(type, broadcast))
            assertEquals(Reading.AS_IS, BlurayStreams.reading(type, null))
            assertEquals(Reading.AS_IS, BlurayStreams.reading(type, ByteArray(0)))
        }
    }

    @Test
    fun aStreamKnownToBeABluraysNeedsNoDescriptor() {
        // Not every disc's streams carry the registration; the server's word
        // that the film came off a disc is enough.
        assertEquals(Reading.DTS_CORE, BlurayStreams.reading(0x86, null, bluray = true))
        assertEquals(Reading.DTS_CORE, BlurayStreams.reading(0x82, ByteArray(0), bluray = true))
        assertEquals(Reading.IGNORE, BlurayStreams.reading(0x80, broadcast, bluray = true))
        assertEquals(Reading.AS_IS, BlurayStreams.reading(0x24, null, bluray = true))
    }

    @Test
    fun theServersMarkOnALinkIsRead() {
        assertTrue(BlurayStreams.isBlurayLink("http://nas:8123/stream/TICKET/Film.ts?disc=bluray"))
        assertTrue(BlurayStreams.isBlurayLink("http://nas:8123/stream/TICKET/Film.ts?from=replica&disc=bluray#t=5"))
        assertFalse(BlurayStreams.isBlurayLink("http://nas:8123/stream/TICKET/Film.ts"))
        assertFalse(BlurayStreams.isBlurayLink("http://nas:8123/stream/TICKET/Film.ts?disc=dvd"))
        // In the name of a film, or after the fragment, it is not a mark.
        assertFalse(BlurayStreams.isBlurayLink("http://nas:8123/stream/TICKET/disc=bluray.ts"))
        assertFalse(BlurayStreams.isBlurayLink("http://nas:8123/stream/TICKET/Film.ts#disc=bluray"))
        assertFalse(BlurayStreams.isBlurayLink("http://nas:8123/stream/TICKET/Film.ts?xdisc=bluray"))
    }

    @Test
    fun theRegistrationIsFoundAmongOtherDescriptors() {
        assertTrue(BlurayStreams.isBluray(hdmv(0x86)))
        assertTrue(BlurayStreams.isBluray(broadcast + hdmv(0x86)))
        assertFalse(BlurayStreams.isBluray(broadcast))
        // "HDMV" as the contents of some other descriptor is not a registration.
        assertFalse(BlurayStreams.isBluray(byteArrayOf(0x0a, 0x04, 'H'.code.toByte(), 'D'.code.toByte(), 'M'.code.toByte(), 'V'.code.toByte())))
    }

    @Test
    fun damagedDescriptorsAreNotABluray() {
        // A length that runs off the end, a lone tag, and noise of every length.
        assertFalse(BlurayStreams.isBluray(byteArrayOf(0x05, 0x40, 'H'.code.toByte(), 'D'.code.toByte())))
        assertFalse(BlurayStreams.isBluray(byteArrayOf(0x05)))
        val random = Random(1)
        for (n in 0..200) BlurayStreams.isBluray(ByteArray(n).also { random.nextBytes(it) })
    }

    // --- DTS ---------------------------------------------------------------

    /** A DTS core frame of [length] bytes: the sync word, the length, and filler that says where it is from. */
    private fun core(length: Int, mark: Int): ByteArray {
        val f = ByteArray(length) { (mark + it).toByte() }
        f[0] = 0x7f; f[1] = 0xfe.toByte(); f[2] = 0x80.toByte(); f[3] = 0x01
        val size = length - 1
        f[4] = 0xfc.toByte()
        f[5] = (0xfc or (size shr 12 and 0x03)).toByte()
        f[6] = (size shr 4 and 0xff).toByte()
        f[7] = ((size and 0x0f shl 4) or 0x0f).toByte()
        return f
    }

    /**
     * The extension that follows a core: its own sync word, its length, and
     * filler. [wide] is the form with the longer length fields.
     */
    private fun extension(length: Int, mark: Int, wide: Boolean = length > 65536): ByteArray {
        val f = ByteArray(length) { (mark - it).toByte() }
        f[0] = 0x64; f[1] = 0x58; f[2] = 0x20; f[3] = 0x25
        for (i in 4..9) f[i] = 0
        val size = length - 1
        if (wide) {
            f[5] = 0x20 // the wide form; then 12 bits of header length, 20 of this
            put(f, 43 + 12, 20, size)
        } else {
            put(f, 43 + 8, 16, size)
        }
        return f
    }

    private fun put(b: ByteArray, from: Int, count: Int, value: Int) {
        for (i in 0 until count) {
            val bit = value shr (count - 1 - i) and 1
            val at = from + i
            b[at / 8] = (b[at / 8].toInt() and (1 shl (7 - at % 8)).inv() or (bit shl (7 - at % 8))).toByte()
        }
    }

    @Test
    fun aFramesLengthIsReadFromItsHeader() {
        assertEquals(2013, BlurayStreams.dtsCoreLength(core(2013, 0)))
        assertEquals(96, BlurayStreams.dtsCoreLength(core(96, 0)))
        assertEquals(16384, BlurayStreams.dtsCoreLength(core(16384, 0)))
        assertEquals(3000, BlurayStreams.dtsExtensionLength(extension(3000, 0)))
        assertEquals(65536, BlurayStreams.dtsExtensionLength(extension(65536, 0)))
        assertEquals(3000, BlurayStreams.dtsExtensionLength(extension(3000, 0, wide = true)))
        assertEquals(700_000, BlurayStreams.dtsExtensionLength(extension(700_000, 0)))

        // Each is only itself, a frame too short to be one is not one, and
        // neither is something too short to tell.
        assertEquals(0, BlurayStreams.dtsCoreLength(extension(500, 0)))
        assertEquals(0, BlurayStreams.dtsExtensionLength(core(500, 0)))
        assertEquals(0, BlurayStreams.dtsCoreLength(core(96, 0).also { it[5] = 0xfc.toByte(); it[6] = 0; it[7] = 0x1f }))
        assertEquals(0, BlurayStreams.dtsCoreLength(byteArrayOf(0x7f, 0xfe.toByte(), 0x80.toByte(), 0x01)))
        assertEquals(0, BlurayStreams.dtsExtensionLength(byteArrayOf(0x64, 0x58, 0x20, 0x25)))
    }

    /** Runs packets through a cut, each handed over in pieces of [piece] bytes, and returns what was kept. */
    private fun cut(packets: List<ByteArray>, piece: Int): ByteArray {
        val cut = DtsCoreCut()
        val kept = java.io.ByteArrayOutputStream()
        for (packet in packets) {
            cut.packetStarted()
            var at = 0
            while (at < packet.size) {
                val n = minOf(piece, packet.size - at)
                // Inside a larger array, as a reader's buffer is.
                val buffer = ByteArray(at + n + 3)
                System.arraycopy(packet, at, buffer, 3, n)
                cut.feed(buffer, 3, n) { bytes, offset, length -> kept.write(bytes, offset, length) }
                at += n
            }
        }
        return kept.toByteArray()
    }

    private val pieces = listOf(1_000_000, 184, 7, 1)

    @Test
    fun onlyTheCoreOfEachFrameIsKept() {
        // A disc's packets: a core and its extension in each.
        val cores = listOf(core(1509, 10), core(2013, 20), core(1509, 30))
        val packets = cores.mapIndexed { i, c -> c + extension(3000 + i * 700, i) }
        val want = cores.reduce { a, b -> a + b }

        // However the bytes arrive: all at once, a transport packet's worth at
        // a time, or a byte at a time.
        for (piece in pieces) assertArrayEquals("in pieces of $piece", want, cut(packets, piece))
    }

    @Test
    fun framesNeedNotLineUpWithPackets() {
        val cores = (0 until 9).map { core(1024 + it * 3, it * 7) }
        val want = cores.reduce { a, b -> a + b }

        // Plain DTS, several frames to a packet: every one is kept.
        val plain = listOf(cores[0] + cores[1] + cores[2], cores[3] + cores[4], cores[5] + cores[6] + cores[7] + cores[8])
        for (piece in pieces) assertArrayEquals("plain, in pieces of $piece", want, cut(plain, piece))

        // DTS-HD, all of it in one packet: each extension says where the next
        // core begins.
        val stream = cores.mapIndexed { i, c -> c + extension(2000 + i * 501, i, wide = i % 2 == 0) }.reduce { a, b -> a + b }
        for (piece in pieces) assertArrayEquals("DTS-HD, in pieces of $piece", want, cut(listOf(stream), piece))
    }

    @Test
    fun aFrameThatRunsIntoTheNextPacketIsFollowedThere() {
        val a = core(2013, 1)
        val b = core(1509, 2)
        val c = core(1509, 3)
        val ext = extension(5000, 9)
        // The first core is split across two packets, early in its body and
        // late; then its extension is, with the next core straight after it.
        for (split in listOf(10, 700, 2012)) {
            val packets = listOf(a.copyOfRange(0, split), a.copyOfRange(split, a.size) + extension(800, 0), b + extension(800, 1))
            assertArrayEquals("core split at $split", a + b, cut(packets, 184))
        }
        for (split in listOf(10, 2500, 4999)) {
            val packets = listOf(a + ext.copyOfRange(0, split), ext.copyOfRange(split, ext.size) + b + extension(900, 2), c)
            assertArrayEquals("extension split at $split", a + b + c, cut(packets, 184))
        }
    }

    @Test
    fun whatIsNotAFrameIsLetGoUntilTheNextPacket() {
        val a = core(1509, 1)
        val b = core(1509, 2)
        val noise = ByteArray(4000) { (it * 31).toByte() }
        // Noise, a packet too short to tell, and noise after a good frame.
        val kept = cut(listOf(a + extension(900, 0), noise, ByteArray(3), b + noise, ByteArray(0), a), 184)
        assertArrayEquals(a + b + a, kept)
    }

    @Test
    fun aSeekStartsAfresh() {
        val cut = DtsCoreCut()
        val kept = java.io.ByteArrayOutputStream()
        val a = core(1509, 1)
        // Half a core, then the player jumps somewhere else.
        cut.packetStarted()
        cut.feed(a, 0, 600) { bytes, offset, length -> kept.write(bytes, offset, length) }
        kept.reset()
        cut.reset()

        val b = core(1509, 2)
        cut.packetStarted()
        cut.feed(b, 0, b.size) { bytes, offset, length -> kept.write(bytes, offset, length) }
        assertArrayEquals(b, kept.toByteArray())
    }
}
