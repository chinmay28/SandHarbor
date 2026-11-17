/* The phone's Back button, inside the Android app.

   The page keeps where you have been in memory, not in the browser's history
   (see navigation.js for why), so the web view has no history of its own to
   step back through and the app, left to itself, quits on the first Back. So
   the app asks the page first: Back undoes the nearest thing in front of you
   — a dialog, then the accounts drawer, then the last folder you walked into
   — and only when there is nothing left to undo does the app go away.

   In a browser there is no window.sandApp and none of this runs; the
   browser's own Back leaves the page, as it always has.

   The hook that wires this to the app is useNativeBack.js; this file is the
   decision alone, so it can be tested without React or a page. */

/* What one press of Back does, given what is on screen. Pure, so the order
   can be tested without a page:

   - 'dialog': a dialog is open; it closes, exactly as Escape would close it.
   - 'drawer': the accounts drawer covers the phone's screen; it closes.
   - 'back':   a folder you came from is behind you; you step back to it.
   - 'up':     nothing behind you but you are not at the top (the folder you
               were in went away, or the vault was unlocked deeper in); you go
               up one.
   - null:     nothing to undo. The app is left to do what Back does there. */
export function backStep({ dialogOpen = false, drawerOpen = false, canBack = false, canUp = false } = {}) {
  if (dialogOpen) return 'dialog'
  if (drawerOpen) return 'drawer'
  if (canBack) return 'back'
  if (canUp) return 'up'
  return null
}
