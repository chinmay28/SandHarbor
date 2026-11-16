/* The one thumbnail backfill this page is running, as seen by everything that
   draws it.

   The dialog that starts it closes straight away and the run carries on here,
   at module level, so the person can go on browsing — into other folders too
   — while it works. The strip above the file list (ThumbBackfill.jsx) is how
   it shows from then on: a bar while it goes, and a line saying what it came
   to once it lands.

   It runs in this tab, not on the server, because only a browser can draw
   these pictures (see thumbs.js). Closing the tab ends it. Nothing is lost by
   that beyond the pictures not yet sent: a file that got its picture keeps it,
   and pressing the button again plans from the listing as it now stands, so it
   carries on from where it was.

   One at a time. Two would race each other over the same folder's pack, and
   be twice the downloads for nothing. */

import { useEffect, useState } from 'react'
import { api } from './api'
import { makeThumbnail } from './thumbs'
import { runBackfill } from './thumbBackfill'

let job = null
let controller = null
let serial = 0
const listeners = new Set()

function publish(next) {
  job = next
  for (const listener of listeners) listener(job)
}

/* Fetches a file as a download would and draws its picture. The fetch is the
   ordinary content endpoint, so a file comes back exactly as opening it would
   bring it back. */
async function makeFromVault(file, signal) {
  const resp = await fetch(api.contentURL(file.id), { credentials: 'same-origin', signal })
  if (!resp.ok) throw new Error(`could not read ${file.name} (${resp.status})`)
  const blob = await resp.blob()
  return makeThumbnail(blob, file.mime, file.name)
}

/* Starts a run over `files` (planned by planBackfill from the folder at
   `path`). Returns the run, or null when one is already going — the dialog
   shows that one instead. */
export function startThumbJob({ path, vault = '', files }) {
  if (job?.running) return null
  serial += 1
  controller = new AbortController()
  const id = serial
  const startedAt = Date.now()
  publish({
    id, path, vault, startedAt, running: true, stopping: false,
    counts: { total: files.length, done: 0, made: 0, stored: 0, skipped: 0, failed: 0, stopped: false, error: null },
  })

  const update = (counts) => {
    if (job?.id !== id) return
    publish({ ...job, counts })
  }

  runBackfill({
    files,
    make: makeFromVault,
    store: (pictures) => api.putThumbs(pictures),
    onProgress: update,
    signal: controller.signal,
  }).then((counts) => {
    if (job?.id !== id) return
    publish({ ...job, counts, running: false, stopping: false, finishedAt: Date.now() })
  })

  return job
}

/* Stops the run between files. What was drawn is still sent; see runBackfill. */
export function stopThumbJob() {
  if (!job?.running) return
  controller?.abort()
  publish({ ...job, stopping: true })
}

/* Forgets a finished run, once its outcome has been read. */
export function dismissThumbJob() {
  if (job && !job.running) publish(null)
}

export function currentThumbJob() {
  return job
}

/* The run, kept current for as long as the caller is on screen. */
export function useThumbJob() {
  const [current, setCurrent] = useState(job)
  useEffect(() => {
    listeners.add(setCurrent)
    setCurrent(job)
    return () => { listeners.delete(setCurrent) }
  }, [])
  return current
}
