/* The width a PDF page is drawn to fit, given the one it was last drawn at
   and the one just measured.

   Redrawing on every change of width is a feedback loop waiting for the
   right page. The viewer sits in a dialog that scrolls when it is taller
   than the screen, and this app styles its scrollbars, which turns them from
   overlays into real 10px gutters — in Chrome, and in the Android app's web
   view. So: the page is drawn a little too tall, the dialog grows a
   scrollbar, the viewer loses 10px, the page is redrawn narrower and so
   shorter, the dialog no longer needs its scrollbar, the viewer gets its
   10px back, the page is redrawn wider — and round again, forever, blanking
   the canvas each time. On a phone, where a portrait page fitted to the
   width is almost exactly as tall as the room it is given, that was most
   PDFs.

   So the two directions are not treated alike. Narrower always wins (beyond
   a few pixels of noise): a page wider than its viewer is cut off or scrolls
   sideways. Wider has to be worth it — more than any scrollbar is wide — so a
   scrollbar coming and going cannot drive a redraw, and a page that is a
   scrollbar's width short of the edge is the whole cost. A real resize, a
   phone turned on its side or a window dragged out, clears that easily. */

/* Below this a change is rounding, or a phone's address bar sliding away. */
export const SHRINK_SLOP = 4

/* Wider than any scrollbar, classic or styled. */
export const GROW_SLOP = 24

export function nextFitWidth(prev, measured) {
  // Hidden, or not laid out yet: nothing to fit, so keep what there is.
  if (!(measured > 0)) return prev
  if (!(prev > 0)) return measured
  const delta = measured - prev
  if (delta < 0) return -delta < SHRINK_SLOP ? prev : measured
  return delta < GROW_SLOP ? prev : measured
}
