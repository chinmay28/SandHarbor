import React, { useEffect, useRef } from 'react'
import { COLORS } from '../theme'
import { Banner, Button } from './ui'
import { RunCard } from './BackgroundRuns'
import { extractRunsLoaded, stopExtract, useExtractRuns } from '../extract'
import { extractFraction, extractNotes, extractOutcomeText, extractProgressText } from '../upload'

/* Every archive the machine is unpacking, drawn above the file list — and
   what one lately came to, until it is dismissed.

   This is where an archive goes once its upload has been answered. The
   unpacking is the machine's work and carries on with this page closed, so
   the strip is its bar on whichever page is open while it goes, and its
   report on whichever is open when it is done.

   The listing behind it is drawn from the index, which the run is adding to
   file by file. So it is refreshed as the count of stored files moves — at a
   pace, since a listing a second is nobody's idea of a progress bar — and
   once more when a run ends. */
const REFRESH_EVERY_MS = 5000

export default function ExtractRuns({ mobile, onChanged }) {
  const runs = useExtractRuns()

  /* Which finished runs have been accounted for, seeded from the first answer
     so a page opened onto an old result does not refresh a listing that was
     just drawn; and how many files had been stored the last time it was. */
  const known = useRef(null)
  const seen = useRef({ stored: 0, at: 0 })
  useEffect(() => {
    if (known.current === null) {
      if (!extractRunsLoaded()) return
      known.current = new Set(runs.filter((run) => run.done).map((run) => run.id))
      return
    }
    let changed = false
    for (const run of runs) {
      if (!run.done || known.current.has(run.id)) continue
      known.current.add(run.id)
      changed = true
    }
    const stored = runs.reduce((sum, run) => sum + (run.done ? 0 : run.at?.files || 0), 0)
    const now = Date.now()
    if (stored !== seen.current.stored && now - seen.current.at >= REFRESH_EVERY_MS) changed = true
    if (changed) {
      seen.current = { stored, at: now }
      onChanged?.()
    }
  }, [runs]) // eslint-disable-line react-hooks/exhaustive-deps

  if (runs.length === 0) return null

  const stop = (id) => { stopExtract(id).catch(() => {}) }

  return (
    <div style={{ padding: mobile ? '10px 10px 0' : '12px 20px 0', flexShrink: 0 }}>
      {runs.map((run) => (run.done
        ? <FinishedExtract key={run.id} run={run} onDismiss={() => stop(run.id)} />
        : (
          <RunCard
            key={run.id}
            title={`${run.waiting ? 'Waiting to extract' : 'Extracting'} ${run.name} into ${run.dir}`}
            label={extractProgressText(run)}
            fraction={run.waiting ? 0 : extractFraction(run)}
            action={<Button size="sm" variant="ghost" onClick={() => stop(run.id)}>Stop</Button>}
          />
        )))}
    </div>
  )
}

/* What an extraction came to, with the first few reasons anything in the
   archive is not in the folder. */
function FinishedExtract({ run, onDismiss }) {
  const notes = extractNotes(run)
  const tone = run.error ? 'error' : run.cancelled || notes.length ? 'warn' : 'success'
  return (
    <Banner tone={tone} onDismiss={onDismiss}>
      <div>{extractOutcomeText(run)}</div>
      {notes.map((line, i) => (
        <div key={i} style={{ marginTop: '4px', fontSize: '11px', color: COLORS.textDim, overflowWrap: 'anywhere' }}>
          {line}
        </div>
      ))}
    </Banner>
  )
}
