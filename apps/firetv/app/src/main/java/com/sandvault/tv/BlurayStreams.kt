package com.sandvault.tv

import kotlin.math.min

/**
 * What the streams of a Blu-ray's transport stream are, worked out from plain
 * values so that it can be tested without a device.
 *
 * A Blu-ray numbers its kinds of stream its own way, and three of its numbers
 * mean something else to Media3, which reads them the way American broadcast
 * does:
 *
 *  - 0x86, DTS-HD Master Audio — the main soundtrack of most discs — is read
 *    as advertising-splice data. The film's sound becomes a track of nonsense
 *    "metadata", which is worse than silence: decoding it can stop the film.
 *  - 0x82, plain DTS, is ignored unless asked for.
 *  - 0x80, uncompressed PCM sound, is read as MPEG-2 video.
 *
 * So the player has to know it is reading a disc. It is told in one of two
 * ways. The server marks the link of a film it is playing out of a Blu-ray
 * image ([isBlurayLink]), which is how every disc it serves is known. And a
 * stream may say so itself, with a registration descriptor reading "HDMV" in
 * the program table ([isBluray]), which is how a Blu-ray rip kept as a .ts is
 * known when its author wrote one. Told neither, nothing here changes how a
 * stream is read: off the air, 0x86 really is splice data.
 */
object BlurayStreams {

    /** How the player should read one stream of a transport stream. */
    enum class Reading {
        /** As Media3 reads it already. */
        AS_IS,

        /** Not at all: nothing here can play it, and reading it as Media3 would is wrong. */
        IGNORE,

        /** As DTS, keeping the core of each frame and leaving out its extension. See [DtsCoreCut]. */
        DTS_CORE,
    }

    const val TYPE_LPCM = 0x80
    const val TYPE_DTS = 0x82
    const val TYPE_DTS_HD_HIGH_RESOLUTION = 0x85
    const val TYPE_DTS_HD_MASTER_AUDIO = 0x86

    /** The mark the server puts in the query of a link to a film out of a Blu-ray image. */
    const val LINK_MARK = "disc=bluray"

    /** Whether a stream link is marked as a film out of a Blu-ray image. */
    fun isBlurayLink(url: String): Boolean =
        url.substringBefore('#').substringAfter('?', "").split('&').any { it == LINK_MARK }

    /**
     * How to read a stream of the given type, whose descriptors in the program
     * table are [descriptors] (Media3's EsInfo.descriptorBytes). [bluray] is
     * whether the file is already known to be a Blu-ray's.
     */
    fun reading(streamType: Int, descriptors: ByteArray?, bluray: Boolean = false): Reading {
        if (!bluray && !isBluray(descriptors)) return Reading.AS_IS
        return when (streamType) {
            TYPE_DTS, TYPE_DTS_HD_HIGH_RESOLUTION, TYPE_DTS_HD_MASTER_AUDIO -> Reading.DTS_CORE
            TYPE_LPCM -> Reading.IGNORE
            else -> Reading.AS_IS
        }
    }

    /** Whether a stream's descriptors include a Blu-ray's registration: tag 5, reading "HDMV". */
    fun isBluray(descriptors: ByteArray?): Boolean {
        if (descriptors == null) return false
        var at = 0
        // A run of descriptors: a tag, a length, and that many bytes, repeated.
        while (at + 2 <= descriptors.size) {
            val tag = descriptors[at].toInt() and 0xff
            val length = descriptors[at + 1].toInt() and 0xff
            val body = at + 2
            if (body + length > descriptors.size) return false
            if (tag == 0x05 && length >= 4 &&
                descriptors[body] == 'H'.code.toByte() && descriptors[body + 1] == 'D'.code.toByte() &&
                descriptors[body + 2] == 'M'.code.toByte() && descriptors[body + 3] == 'V'.code.toByte()
            ) return true
            at = body + length
        }
        return false
    }

    /** How much of a DTS frame, or of an extension, has to be seen before its length is known. */
    const val DTS_HEAD_BYTES = 10

    /** No DTS core frame is shorter than this. */
    private const val DTS_SHORTEST_FRAME = 96

    /**
     * The length in bytes of the DTS core frame that begins with [head], or 0
     * when [head] does not begin one. [head] holds at least [DTS_HEAD_BYTES].
     *
     * A core frame opens with the sync word 7F FE 80 01, and its length less
     * one is the fourteen bits that start at bit 46.
     */
    fun dtsCoreLength(head: ByteArray): Int {
        if (head.size < DTS_HEAD_BYTES || word(head) != 0x7FFE8001) return 0
        val length = bits(head, 46, 14) + 1
        return if (length < DTS_SHORTEST_FRAME) 0 else length
    }

    /**
     * The length in bytes of the DTS extension that begins with [head] — what
     * follows a core frame in DTS-HD — or 0 when [head] does not begin one.
     *
     * An extension opens with the sync word 64 58 20 25. Eleven bits on, one
     * bit says how wide the next two fields are: its header's length and then
     * its own, each less one, in 8 and 16 bits or in 12 and 20.
     */
    fun dtsExtensionLength(head: ByteArray): Int {
        if (head.size < DTS_HEAD_BYTES || word(head) != 0x64582025) return 0
        val wide = bits(head, 42, 1) == 1
        val length = if (wide) bits(head, 43 + 12, 20) + 1 else bits(head, 43 + 8, 16) + 1
        return if (length < DTS_HEAD_BYTES) 0 else length
    }

    private fun word(b: ByteArray): Int = bits(b, 0, 32)

    /** [count] bits of [b], most significant first, starting [from] bits in. */
    private fun bits(b: ByteArray, from: Int, count: Int): Int {
        var value = 0
        for (i in from until from + count) {
            value = (value shl 1) or ((b[i / 8].toInt() shr (7 - i % 8)) and 1)
        }
        return value
    }
}

/**
 * Cuts the DTS core out of a DTS-HD stream as it arrives.
 *
 * A DTS-HD frame is a plain DTS frame — the core, 5.1 at up to 1.5 Mb/s, which
 * every DTS decoder plays — followed by an extension that turns it into the
 * lossless or 7.1 version. Media3's DTS reader takes the two for separate
 * frames of one track and hands a decoder each on its own, which no decoder
 * here can use. Given only the cores it reads an ordinary DTS track.
 *
 * Both say how long they are in their first few bytes. So the stream is
 * walked one piece at a time: a core is kept, an extension is passed over,
 * and each ends where the next begins, however the pieces fall across the
 * packets that carry them. Anything else — the middle of a frame after a
 * seek, or damage — is let go until the next packet starts, which is where a
 * frame begins again.
 */
class DtsCoreCut {

    private enum class At { HEAD, CORE, EXTENSION, LOST }

    private val head = ByteArray(BlurayStreams.DTS_HEAD_BYTES)
    private var headFill = 0
    private var left = 0
    private var at = At.HEAD

    /** Forget everything: the stream is being read from somewhere new. */
    fun reset() {
        at = At.HEAD
        headFill = 0
        left = 0
    }

    /** A packet of the stream is starting. */
    fun packetStarted() {
        // Partway through a core or an extension, the packet carries on with
        // it. Anything else starts again: a few bytes held from a packet too
        // short to be told apart must not be taken for the start of this one.
        if (at == At.HEAD || at == At.LOST) reset()
    }

    /**
     * Takes the next [length] bytes of the stream, at [offset] in [data], and
     * calls [keep] with each run of them that belongs to a core frame, in
     * order. The array handed to [keep] is only good for the length of the call.
     */
    fun feed(data: ByteArray, offset: Int, length: Int, keep: (ByteArray, Int, Int) -> Unit) {
        var pos = offset
        val end = offset + length
        while (pos < end) {
            when (at) {
                At.HEAD -> {
                    val n = min(end - pos, head.size - headFill)
                    System.arraycopy(data, pos, head, headFill, n)
                    headFill += n
                    pos += n
                    if (headFill == head.size) {
                        headFill = 0
                        val core = BlurayStreams.dtsCoreLength(head)
                        val extension = BlurayStreams.dtsExtensionLength(head)
                        when {
                            core > 0 -> {
                                keep(head, 0, head.size)
                                left = core - head.size
                                at = At.CORE
                            }
                            extension > 0 -> {
                                left = extension - head.size
                                at = At.EXTENSION
                            }
                            else -> at = At.LOST
                        }
                        if (left == 0 && at != At.LOST) at = At.HEAD
                    }
                }
                At.CORE, At.EXTENSION -> {
                    val n = min(end - pos, left)
                    if (at == At.CORE) keep(data, pos, n)
                    left -= n
                    pos += n
                    if (left == 0) at = At.HEAD
                }
                At.LOST -> pos = end
            }
        }
    }
}
