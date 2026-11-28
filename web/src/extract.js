/* The archives the machine is unpacking, as seen from the page.

   An archive uploaded to be extracted is only carried here by the page: the
   upload is answered as soon as the archive has arrived, and unpacking it is
   a run on the machine (see extract_runs.go) that goes on whether this page
   stays open or not. This is the page's view of those runs — the same shape
   as erase.js, for the same reason: one poller for the whole page, asked
   often while something is going and only now and then when nothing is. */

import { useEffect, useState } from 'react'
import { api } from './api'

const BUSY_POLL_MS = 1500
const IDLE_POLL_MS = 15000

let runs = []
let asked = false
const listeners = new Set()
let timer = null
let asking = false

function publish(next) {
  runs = next
  for (const listener of listeners) listener(runs)
}

async function ask() {
  if (asking) return
  asking = true
  try {
    const resp = await api.extractRuns()
    asked = true
    publish(resp.runs || [])
  } catch {
    // A poll that failed says nothing about the runs, which are going on the
    // machine either way. Leave the last answer up and ask again later.
  } finally {
    asking = false
    schedule()
  }
}

function schedule() {
  clearTimeout(timer)
  timer = null
  if (listeners.size === 0) return
  const busy = runs.some((run) => !run.done)
  timer = setTimeout(ask, busy ? BUSY_POLL_MS : IDLE_POLL_MS)
}

function subscribe(listener) {
  listeners.add(listener)
  if (listeners.size === 1) ask()
  return () => {
    listeners.delete(listener)
    if (listeners.size === 0) {
      clearTimeout(timer)
      timer = null
    }
  }
}

/* Every run waiting, going or lately finished, kept current for as long as
   something on the page is reading it. */
export function useExtractRuns() {
  const [current, setCurrent] = useState(runs)
  useEffect(() => subscribe(setCurrent), [])
  return current
}

/* Whether the machine has answered at least once since the page loaded. */
export function extractRunsLoaded() {
  return asked
}

/* An upload was just told one of its archives is being extracted: ask now
   rather than at the idle cadence, so its bar is there when the upload's own
   card goes. */
export function noteExtractStarted() {
  if (listeners.size > 0) ask()
}

/* Stop a waiting or running extraction, or dismiss a finished one's result.
   What it had stored stays stored. */
export async function stopExtract(id) {
  const resp = await api.stopExtract(id)
  publish(resp.runs || [])
  schedule()
}
