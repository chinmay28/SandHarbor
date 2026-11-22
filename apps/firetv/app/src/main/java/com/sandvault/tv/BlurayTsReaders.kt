package com.sandvault.tv

import android.net.Uri
import android.util.SparseArray
import androidx.annotation.OptIn
import androidx.media3.common.MimeTypes
import androidx.media3.common.util.ParsableByteArray
import androidx.media3.common.util.TimestampAdjuster
import androidx.media3.common.util.UnstableApi
import androidx.media3.extractor.DefaultExtractorsFactory
import androidx.media3.extractor.Extractor
import androidx.media3.extractor.ExtractorOutput
import androidx.media3.extractor.ExtractorsFactory
import androidx.media3.extractor.text.DefaultSubtitleParserFactory
import androidx.media3.extractor.text.SubtitleParser
import androidx.media3.extractor.ts.DefaultTsPayloadReaderFactory
import androidx.media3.extractor.ts.DtsReader
import androidx.media3.extractor.ts.ElementaryStreamReader
import androidx.media3.extractor.ts.PesReader
import androidx.media3.extractor.ts.TsExtractor
import androidx.media3.extractor.ts.TsPayloadReader

/**
 * Media3's extractors, with the one for transport streams taught what a
 * Blu-ray's streams are (see [BlurayStreams]) and told to look far enough for
 * the film's length.
 *
 * Everything else — every other container, and the subtitle handling the
 * media source asks for — is Media3's own, passed straight through.
 */
@OptIn(UnstableApi::class)
class FilmExtractors(
    /** Whether the film is known to be a Blu-ray's transport stream. See [BlurayStreams]. */
    private val bluray: Boolean = false,
    private val defaults: DefaultExtractorsFactory = DefaultExtractorsFactory(),
) : ExtractorsFactory {

    // What the media source asks of its extractors, kept so that the
    // transport-stream extractor built here is asked the same.
    private var subtitles: SubtitleParser.Factory = DefaultSubtitleParserFactory()
    private var transcodeText = true

    override fun setSubtitleParserFactory(subtitleParserFactory: SubtitleParser.Factory): ExtractorsFactory {
        subtitles = subtitleParserFactory
        defaults.setSubtitleParserFactory(subtitleParserFactory)
        return this
    }

    @Deprecated("Media3 calls it, so it has to be passed on.")
    override fun experimentalSetTextTrackTranscodingEnabled(textTrackTranscodingEnabled: Boolean): ExtractorsFactory {
        transcodeText = textTrackTranscodingEnabled
        @Suppress("DEPRECATION")
        defaults.experimentalSetTextTrackTranscodingEnabled(textTrackTranscodingEnabled)
        return this
    }

    override fun experimentalSetCodecsToParseWithinGopSampleDependencies(codecs: Int): ExtractorsFactory {
        defaults.experimentalSetCodecsToParseWithinGopSampleDependencies(codecs)
        return this
    }

    override fun createExtractors(): Array<Extractor> = withOurs(defaults.createExtractors())

    override fun createExtractors(uri: Uri, responseHeaders: Map<String, List<String>>): Array<Extractor> =
        withOurs(defaults.createExtractors(uri, responseHeaders))

    /** The same extractors in the same order, with the transport-stream one replaced. */
    private fun withOurs(all: Array<Extractor>): Array<Extractor> =
        Array(all.size) { i -> if (all[i].underlyingImplementation is TsExtractor) transportStream() else all[i] }

    private fun transportStream(): Extractor = TsExtractor(
        TsExtractor.MODE_SINGLE_PMT,
        if (transcodeText) 0 else TsExtractor.FLAG_EMIT_RAW_SUBTITLE_DATA,
        subtitles,
        TimestampAdjuster(0),
        BlurayTsReaders(bluray),
        TS_CLOCK_SEARCH_BYTES,
    )

    companion object {
        /**
         * How much of each end of a transport stream is searched for a clock
         * reference, which is the only way one says how long it is: 12,000
         * packets, a little over 2 MB.
         *
         * Media3 looks in the first and last 113 KB. That is ten milliseconds
         * of a UHD Blu-ray, which carries a reference every hundred: none is
         * found, and the film has no length and cannot be seeked or resumed.
         * At the 128 Mb/s a UHD disc may reach, references are 1.6 MB apart.
         */
        const val TS_CLOCK_SEARCH_BYTES = 188 * 12_000
    }
}

/** Media3's readers for the streams of a transport stream, except where a Blu-ray means something else. */
@OptIn(UnstableApi::class)
class BlurayTsReaders(
    private val bluray: Boolean = false,
    private val base: TsPayloadReader.Factory = DefaultTsPayloadReaderFactory(),
) : TsPayloadReader.Factory {

    override fun createInitialPayloadReaders(): SparseArray<TsPayloadReader> = base.createInitialPayloadReaders()

    override fun createPayloadReader(streamType: Int, esInfo: TsPayloadReader.EsInfo): TsPayloadReader? =
        when (BlurayStreams.reading(streamType, esInfo.descriptorBytes, bluray)) {
            BlurayStreams.Reading.AS_IS -> base.createPayloadReader(streamType, esInfo)
            BlurayStreams.Reading.IGNORE -> null
            BlurayStreams.Reading.DTS_CORE -> PesReader(
                DtsCoreReader(DtsReader(esInfo.language, esInfo.roleFlags, DTS_EXTENSION_HEADER_MOST, MimeTypes.VIDEO_MP2T)),
            )
        }

    private companion object {
        /** The most a DTS extension header can be, which Media3's reader asks to be told. */
        const val DTS_EXTENSION_HEADER_MOST = 4096
    }
}

/** A DTS reader that is shown only the core of each frame. See [DtsCoreCut]. */
@OptIn(UnstableApi::class)
class DtsCoreReader(private val inner: ElementaryStreamReader) : ElementaryStreamReader {

    private val cut = DtsCoreCut()

    override fun seek() {
        cut.reset()
        inner.seek()
    }

    override fun createTracks(extractorOutput: ExtractorOutput, idGenerator: TsPayloadReader.TrackIdGenerator) =
        inner.createTracks(extractorOutput, idGenerator)

    override fun packetStarted(pesTimeUs: Long, flags: Int) {
        cut.packetStarted()
        inner.packetStarted(pesTimeUs, flags)
    }

    override fun consume(data: ParsableByteArray) {
        cut.feed(data.data, data.position, data.bytesLeft()) { bytes, offset, length ->
            val kept = ParsableByteArray(bytes, offset + length)
            kept.position = offset
            inner.consume(kept)
        }
        data.position = data.limit()
    }

    override fun packetFinished(isEndOfInput: Boolean) = inner.packetFinished(isEndOfInput)
}
