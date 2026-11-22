package com.sandvault.tv

import androidx.media3.common.Player
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ScreenAwakeTest {

    @Test
    fun aPlayingFilmHoldsTheScreen() {
        assertTrue(ScreenAwake.whilePlaying(playWhenReady = true, playbackState = Player.STATE_READY))
    }

    @Test
    fun aFilmFillingItsBufferHoldsTheScreen() {
        // The start of a film, a seek, or a slow link: nothing is moving on
        // screen, but somebody is still watching it.
        assertTrue(ScreenAwake.whilePlaying(playWhenReady = true, playbackState = Player.STATE_BUFFERING))
    }

    @Test
    fun aPausedFilmLetsTheScreensaverCome() {
        assertFalse(ScreenAwake.whilePlaying(playWhenReady = false, playbackState = Player.STATE_READY))
        assertFalse(ScreenAwake.whilePlaying(playWhenReady = false, playbackState = Player.STATE_BUFFERING))
    }

    @Test
    fun aFilmThatIsOverOrNeverStartedLetsTheScreensaverCome() {
        for (playWhenReady in listOf(true, false)) {
            assertFalse(ScreenAwake.whilePlaying(playWhenReady, Player.STATE_ENDED))
            assertFalse(ScreenAwake.whilePlaying(playWhenReady, Player.STATE_IDLE))
        }
    }
}
