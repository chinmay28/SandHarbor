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

/* How many times a file or a batch is tried when the server answers that it
   is busy or briefly broken (RETRY_STATUSES), before that one is given up. */
export const BACKFILL_TRIES = 4

/* Answers that say "not now" rather than "not this file". */
const RETRY_STATUSES = new Set([408, 425, 429, 500, 502, 503, 504])

/* What a thrown error means for the run:

     'offline'  the page could not reach the server at all (err.offline) —
                a backgrounded tab, a phone that slept, Wi-Fi that dropped.
                Nothing is wrong with the file; wait and try it again, for as
                long as it takes.
     'locked'   the server answered 401: the vault locked (its idle timer runs
                out while a backgrounded tab is cut off) or the session ended.
                Nothing is wrong with the file either; wait for somebody to
                unlock it, for as long as it takes, and carry on.
     'retry'    the server is busy or briefly broken; try a few more times.
     'fail'     this one file (or batch) cannot be done; count it and move on.

   Nothing ends a run but Stop. */
export function classifyError(err) {
  if (err?.offline) return 'offline'
  const status = err?.status
  if (status === 401) return 'locked'
  if (RETRY_STATUSES.has(status)) return 'retry'
  return 'fail'
}

/* The wait before the attempt after `attempt`: 1s, 2s, 4s… up to 30s. */
export function backoffDelay(attempt) {
  return Math.min(1000 * 2 ** Math.max(0, attempt - 1), 30000)
}

/* Waits backoffDelay(attempt), or less if `signal` aborts. */
export function sleepPause(attempt, signal) {
  return new Promise((resolve) => {
    if (signal?.aborted) return resolve()
    const done = () => {
      clearTimeout(timer)
      signal?.removeEventListener('abort', done)
      resolve()
    }
    const timer = setTimeout(done, backoffDelay(attempt))
    signal?.addEventListener('abort', done)
  })
}

/* How many files of a finished run did not get a thumbnail, whatever the
   reason: not read, not drawn, or drawn and not stored. */
export function failedTotal(c) {
  return (c.failed || 0) + (c.skipped || 0) + (c.refused || 0)
}

/* Works through `files`, a few at a time, and stores the pictures in batches.

     make(file, signal)  → Promise<Blob|null>  a picture, or null where none
                                               can be drawn. A throw is read by
                                               classifyError.
     store(pictures)     → Promise<{stored}>   keeps a Map of id → Blob. Only
                                               one is ever in flight. A throw
                                               is read by classifyError too.
     pause(attempt, signal) → Promise          waits before trying again; it
                                               must resolve when `signal`
                                               aborts. sleepPause by default.
     onProgress(counts)                        after each file and each batch,
                                               and on going into and out of a
                                               wait.

   A run is meant to be left alone, and it goes on until every file has been
   tried. Trouble that passes is waited out: when the page cannot reach the
   server (counts.waiting) or the vault has locked (counts.locked), the file or
   batch in hand is tried again, as many times as it takes, and the run carries
   on from there. A busy server is tried a few times. A file that still cannot
   be read is counted as failed, and a batch the server turns down is counted
   as refused, and either way the run moves on.

   An aborted `signal` stops it taking new files and ends any wait; the files
   in hand are let go, and what was already drawn is still sent once, since it
   costs nothing more to keep. It resolves — never rejects — with the counts
   it came to:

     { total, done, made, stored, refused, skipped, failed, waiting, locked, stopped } */
export async function runBackfill({
  files, make, store, onProgress, signal, pause = sleepPause,
  lanes = BACKFILL_LANES, batch = BACKFILL_BATCH, tries = BACKFILL_TRIES,
}) {
  const counts = {
    total: files.length, done: 0, made: 0, stored: 0, refused: 0,
    skipped: 0, failed: 0, waiting: false, locked: false, stopped: false,
  }
  const report = () => onProgress?.({ ...counts })
  const halted = () => !!signal?.aborted

  /* How many lanes, and the store, are waiting on the connection or on an
     unlock. */
  const waiters = { offline: 0, locked: 0 }
  const setWaiting = (kind, delta) => {
    waiters[kind] += delta
    const waiting = waiters.offline > 0
    const locked = waiters.locked > 0
    if (waiting !== counts.waiting || locked !== counts.locked) {
      counts.waiting = waiting
      counts.locked = locked
      report()
    }
  }

  /* Runs `fn` until it succeeds, or until classifyError says to stop trying;
     then throws the last error. */
  const persist = async (fn) => {
    for (let attempt = 1; ; attempt += 1) {
      try {
        return await fn()
      } catch (err) {
        const kind = classifyError(err)
        const forever = kind === 'offline' || kind === 'locked'
        const again = forever || (kind === 'retry' && attempt < tries)
        if (!again || halted()) throw err
        if (forever) setWaiting(kind, 1)
        try {
          await pause(attempt, signal)
        } finally {
          if (forever) setWaiting(kind, -1)
        }
        if (halted()) throw err
      }
    }
  }

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
      try {
        const resp = await persist(() => store(chunk))
        const stored = typeof resp?.stored === 'number' ? resp.stored : chunk.size
        counts.stored += stored
        // Turned down one by one: the server could not read those pictures.
        counts.refused += Math.max(0, chunk.size - stored)
      } catch {
        counts.refused += chunk.size
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
        picture = await persist(() => make(file, signal))
      } catch {
        failed = true
      }
      // A file let go because the run was stopped is neither made nor failed.
      if (halted()) return
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
