/* Apple's edit sidecars.

   An .aae file is what iOS and Photos write beside a picture when it has been
   edited: a small XML property list naming the adjustments — crop, filter,
   light — and nothing of the picture itself. Exported from a phone, a folder
   of photographs is full of them:

     IMG_1234.HEIC    the picture as it was taken
     IMG_E1234.HEIC   the same picture with the edits drawn in, when exported
     IMG_O1234.AAE    the edits themselves
     IMG_5678.HEIC    a picture exported as the original…
     IMG_5678.AAE     …with its edits beside it

   Opening one is almost always a question about the picture it belongs to, so
   this finds that picture among the folder's images. The edits are Apple's own
   format and are not re-applied here; where the export carried the edited
   rendering (the _E file) that is the one offered, since it is what the edits
   look like. */

const SIDECAR = /\.aae$/i

export function isSidecar(name = '') {
  return SIDECAR.test(name)
}

/* The picture a sidecar belongs to, as an entry of `gallery` — the folder's
   images, each `{ file, hasThumb }` — or null when none of them is it. Names
   are compared without regard to case, since a phone writes IMG_1234.HEIC and
   a sidecar exported elsewhere can come back as img_1234.aae. */
export function photoFor(name = '', gallery = []) {
  if (!isSidecar(name)) return null
  const stems = candidateStems(name.replace(SIDECAR, ''))
  const byStem = new Map()
  for (const entry of gallery) {
    const stem = stemOf(entry?.file?.name || '').toLowerCase()
    if (stem && !byStem.has(stem)) byStem.set(stem, entry)
  }
  for (const stem of stems) {
    const hit = byStem.get(stem)
    if (hit) return hit
  }
  return null
}

/* The stems a sidecar's picture may have, best first and lowercased. For
   IMG_O1234 or IMG_1234 that is IMG_E1234 — the edits drawn in — then
   IMG_1234, the picture as taken. A stem not in Apple's IMG_ shape is only
   itself. */
export function candidateStems(stem) {
  const lower = stem.toLowerCase()
  const apple = /^(.*_)o?(\d+)$/.exec(lower)
  if (!apple) return [lower]
  const [, prefix, number] = apple
  const stems = [`${prefix}e${number}`, `${prefix}${number}`]
  if (!stems.includes(lower)) stems.push(lower)
  return stems
}

function stemOf(name) {
  const dot = name.lastIndexOf('.')
  return dot > 0 ? name.slice(0, dot) : name
}
