/* Making the thumbnails a folder is missing, as one run.

   A file uploaded before thumbnails existed, or from the command line, or
   through WebDAV, has no picture until somebody opens it. This is the run that
   gives a whole folder theirs at once: fetch each file, draw its picture, and
   send the pictures back to be stored.

   It runs in the browser for the reason the upload path does (see thumbs.js):
   HEIC, EXIF orientation and the first page of a PDF can only be drawn here.

   Nothing in this module touches the network, the DOM or React. What it plans
   from is a listing, and what it does to each file is handed in — `make` to
   turn a file into a picture and `store` to keep a batch of them — so the
   whole of its behaviour can be tested with plain functions. thumbJob.js wires
   it to the real API.

   A run covers a folder and everything below it: the plan comes from one
   answer for the whole tree, and the pictures go up by file id, so it does not
   matter to the run which folder each file sits in. */

/* How many files are fetched and drawn at once. Each one is the whole file in
   memory and then its decoded pixels, so this stays small; the work is bound
   by the download and the decode either way. */
export const BACKFILL_LANES = 2

/* How many pictures go up in one request. Each request rewrites the folder's
   pack once, so bigger is cheaper for the server — but a run stopped or closed
   partway loses whatever was made and not yet sent. Matches maxThumbBatch on
   the server, which refuses more. */
export const BACKFILL_BATCH = 32

/* Which files want a picture made, from GET /api/thumbs/missing — the folder
   the run was started on and every folder below it, each as
   { dir, room, files }, `files` already narrowed to the ones with no picture
   and in the order the folder's pack keeps them.

   A file is planned when it is of a kind that can have a picture (`kindOf`,
   which is thumbnailKind), is not so large that drawing it is not worth it,
   and its folder's pack still has room. Room is taken in the order the files
   come, so what is planned is what the server will keep: a picture drawn past
   it would be stored and then trimmed straight back out, and drawn again on
   every run after.

   The files left out are counted, by reason, so the dialog can say why rather
   than leave them unexplained:

     { files, folders, tooLarge, overFull } */
export function planBackfill(missing, kindOf, maxBytes = Infinity) {
  const files = []
  let folders = 0
  let tooLarge = 0
  let overFull = 0
  for (const folder of missing?.folders || []) {
    let room = typeof folder?.room === 'number' ? folder.room : Infinity
    let planned = 0
    for (const file of folder?.files || []) {
      if (!file?.id) continue
      if (!kindOf(file.mime || '', file.name || '')) continue
      if ((file.size || 0) > maxBytes) {
        tooLarge += 1
        continue
      }
      if (room <= 0) {
        overFull += 1
        continue
      }
      room -= 1
      planned += 1
      files.push(file)
    }
    if (planned > 0) folders += 1
  }
  return { files, folders, tooLarge, overFull }
}

/* Works through `files`, a few at a time, and stores the pictures in batches.

     make(file, signal)  → Promise<Blob|null>  a picture, or null where none
                                               can be drawn. A throw is a
                                               failure (the file would not
                                               download, say).
     store(pictures)     → Promise<{stored}>   keeps a Map of id → Blob. Only
                                               one is ever in flight. A throw
                                               ends the run: whatever is wrong
                                               on the server is wrong for the
                                               next batch too, and every file
                                               fetched until then would be a
                                               download wasted.
     onProgress(counts)                        after each file and each batch.

   An aborted `signal` stops it taking new files; the ones in hand are let go,
   and what was already drawn is still sent, since it costs nothing more to
   keep. It resolves — never rejects — with the counts it came to:

     { total, done, made, stored, skipped, failed, stopped, error } */
export async function runBackfill({
  files, make, store, onProgress,
  signal, lanes = BACKFILL_LANES, batch = BACKFILL_BATCH,
}) {
  const counts = {
    total: files.length, done: 0, made: 0, stored: 0,
    skipped: 0, failed: 0, stopped: false, error: null,
  }
  const report = () => onProgress?.({ ...counts })
  const halted = () => signal?.aborted || counts.error !== null

  let pending = new Map()
  let storing = Promise.resolve()

  /* Hands what has been drawn so far to `store`, behind whatever is already
     being stored, so batches never overlap — two at once would each rewrite
     the same pack from the same starting point and one would be lost. */
  const flush = () => {
    if (pending.size === 0) return storing
    const chunk = pending
    pending = new Map()
    storing = storing.then(async () => {
      if (counts.error !== null) return
      try {
        const resp = await store(chunk)
        counts.stored += typeof resp?.stored === 'number' ? resp.stored : chunk.size
      } catch (err) {
        counts.error = err?.message || String(err)
      }
      report()
    })
    return storing
  }

  let next = 0
  const lane = async () => {
    while (!halted() && next < files.length) {
      const file = files[next]
      next += 1
      let picture = null
      let failed = false
      try {
        picture = await make(file, signal)
      } catch {
        failed = true
      }
      // A file let go because the run was stopped is neither made nor failed.
      if (signal?.aborted) return
      counts.done += 1
      if (failed) counts.failed += 1
      else if (!picture) counts.skipped += 1
      else {
        counts.made += 1
        pending.set(file.id, picture)
        if (pending.size >= batch) flush()
      }
      report()
    }
  }

  await Promise.all(Array.from({ length: Math.min(lanes, files.length) }, lane))
  await flush()
  counts.stopped = !!signal?.aborted
  report()
  return { ...counts }
}
