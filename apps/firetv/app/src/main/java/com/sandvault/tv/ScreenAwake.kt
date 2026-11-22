package com.sandvault.tv

import androidx.media3.common.Player

/**
 * Whether the TV must be kept from its screensaver, worked out from plain
 * values, like [BufferPlan], so that it can be tested without a device.
 *
 * A Fire TV starts its screensaver after a few minutes without a press of the
 * remote, and a film is exactly that: nobody touches the remote while it
 * plays. Nothing in Media3 holds the screen for the app — the window has to
 * ask, and it asks only while this says so, so that a film left paused, or
 * one that has ended, still gives way to the screensaver rather than burning a
 * still frame into the panel.
 */
object ScreenAwake {

    /**
     * True while a film is playing or is about to: it has been told to play
     * ([playWhenReady]) and is either playing or filling its buffer
     * ([playbackState], one of Media3's Player.STATE_ values). Buffering
     * counts, since a film waiting on a slow link is still being watched.
     */
    fun whilePlaying(playWhenReady: Boolean, playbackState: Int): Boolean =
        playWhenReady && (playbackState == Player.STATE_READY || playbackState == Player.STATE_BUFFERING)
}
