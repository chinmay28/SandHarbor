import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { MAX_SOURCE_BYTES, thumbnailKind } from '../thumbs'
import { planBackfill } from '../thumbBackfill'
import { dismissThumbJob, startThumbJob, stopThumbJob, useThumbJob } from '../thumbJob'
import { Banner, Button, Modal, Spinner } from './ui'
import { RunCard } from './BackgroundRuns'
import { formatElapsed } from './ScanProgress'

/* "Make missing thumbnails", from the organizer, and the strip that carries the
   run once its dialog is closed.

   The dialog plans from the folder and every folder below it — one request for
   the whole tree — and counts what it would do before offering to do it, as
   every organizer tool does. Starting closes it: the run belongs to the
   page from then on (see thumbJob.js), and the strip above the file list is
   where it shows. */

const plural = (n, one, many = `${one}s`) => `${n.toLocaleString()} ${n === 1 ? one : many}`

export function MakeThumbnails({ path, vault = '', onClose }) {
  const job = useThumbJob()
  const [plan, setPlan] = useState(null)
  const [error, setError] = useState(null)
  const [perFolder, setPerFolder] = useState(0)

  useEffect(() => {
    let live = true
    api.missingThumbs(path, vault)
      .then((missing) => {
        if (!live) return
        setPerFolder(missing?.per_folder || 0)
        setPlan(planBackfill(missing, thumbnailKind, MAX_SOURCE_BYTES))
      })
      .catch((err) => { if (live) setError(err.message) })
    return () => { live = false }
  }, [path, vault])

  const start = () => {
    startThumbJob({ path, vault, files: plan.files })
    onClose()
  }

  const busy = !!job?.running
  const here = path === '/' ? 'the top of the vault' : path

  return (
    <Modal title="Make missing thumbnails" subtitle={path} onClose={onClose} width={460}>
      {error && <Banner tone="error">{error}</Banner>}

      {!error && !plan && (
        <div style={{ padding: '28px', textAlign: 'center' }}><Spinner size={18} /></div>
      )}

      {plan && (
        <>
          <p style={lead}>
            Every picture and PDF in {here}, and in every folder below it, that has no
            thumbnail is read back from its clouds, drawn small in this browser, and its
            thumbnail stored.
          </p>

          {busy ? (
            <Banner tone="info">
              Thumbnails are already being made for {job.path === path && (job.vault || '') === vault ? 'this folder' : job.path}.
              {' '}It shows above the file list, where it can be stopped; start this once it is done.
            </Banner>
          ) : plan.files.length === 0 ? (
            <Banner tone="success">
              Nothing to make — every picture and PDF here and below already has its thumbnail.
            </Banner>
          ) : (
            <Banner tone="info">
              {plural(plan.files.length, 'file')}
              {plan.folders > 1 ? ` in ${plural(plan.folders, 'folder')}` : ' here'}
              {plan.files.length === 1 ? ' has' : ' have'} no thumbnail.
            </Banner>
          )}

          {plan.tooLarge > 0 && (
            <p style={note}>
              {plural(plan.tooLarge, 'file')} over {Math.round(MAX_SOURCE_BYTES / (1024 * 1024))} MB
              {plan.tooLarge === 1 ? ' is' : ' are'} left out: drawing a picture that large costs more than the thumbnail is worth.
            </p>
          )}

          {plan.overFull > 0 && (
            <p style={note}>
              {plural(plan.overFull, 'file')} {plan.overFull === 1 ? 'is' : 'are'} left out: a folder
              keeps at most {perFolder.toLocaleString()} thumbnails, and the ones past that are the
              files it lists last.
            </p>
          )}

          {!busy && plan.files.length > 0 && (
            <p style={note}>
              It runs in the background while you keep browsing, in this tab. If the
              connection drops — as it can while the tab is in the background — it waits and
              carries on. Closing the tab stops it, and running it again carries on from where
              it got to.
            </p>
          )}

          <div style={{ display: 'flex', gap: '10px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
            {!busy && plan.files.length > 0 && (
              <Button variant="primary" onClick={start}>
                Make {plural(plan.files.length, 'thumbnail')}
              </Button>
            )}
            <Button variant="ghost" onClick={onClose}>{busy || plan.files.length === 0 ? 'Close' : 'Cancel'}</Button>
          </div>
        </>
      )}
    </Modal>
  )
}

/* How often, at most, the listing is refreshed while pictures are landing, so
   they appear as the run goes rather than all at the end. */
const REFRESH_MS = 4000

/* The run above the file list: a bar with a Stop while it goes, and what it came
   to once it lands. `onChanged` refreshes the listing, so the new pictures are
   drawn. */
export function ThumbJobStrip({ mobile, onChanged }) {
  const job = useThumbJob()
  const [, tick] = useState(0)

  const running = !!job?.running
  useEffect(() => {
    if (!running) return undefined
    const timer = setInterval(() => tick((n) => n + 1), 1000)
    return () => clearInterval(timer)
  }, [running])

  /* Refresh as batches land, no more often than REFRESH_MS, and once more when
     the run ends so the last batch shows. */
  const lastStored = useRef(0)
  const lastRefresh = useRef(0)
  const stored = job?.counts?.stored || 0
  useEffect(() => {
    if (!job || stored === 0 || stored === lastStored.current) return
    const now = Date.now()
    if (running && now - lastRefresh.current < REFRESH_MS) return
    lastStored.current = stored
    lastRefresh.current = now
    onChanged?.()
  }, [job?.id, stored, running]) // eslint-disable-line react-hooks/exhaustive-deps

  if (!job) return null
  const pad = { padding: mobile ? '10px 10px 0' : '12px 20px 0', flexShrink: 0 }
  const c = job.counts

  if (running) {
    const elapsed = formatElapsed(Date.now() - job.startedAt)
    return (
      <div style={pad}>
        <RunCard
          title={`Making thumbnails in ${job.path}`}
          label={job.stopping
            ? `Stopping — sending the ${plural(c.made - c.stored, 'picture')} already drawn…`
            : c.waiting
              ? `Waiting for the connection — it carries on by itself · ${c.done.toLocaleString()} of ${c.total.toLocaleString()} · ${elapsed}`
              : `${c.done.toLocaleString()} of ${c.total.toLocaleString()} · ${plural(c.stored, 'thumbnail')} stored · ${elapsed}`}
          fraction={c.total ? c.done / c.total : null}
          action={(
            <Button size="sm" variant="ghost" disabled={job.stopping} onClick={stopThumbJob}>
              {job.stopping ? 'Stopping…' : 'Stop'}
            </Button>
          )}
        />
      </div>
    )
  }

  return (
    <div style={pad}>
      <Banner tone={c.error ? 'error' : c.failed ? 'warn' : 'success'} onDismiss={dismissThumbJob}>
        {thumbOutcomeText(job.path, c)}
      </Banner>
    </div>
  )
}

/* The sentence a finished run comes to. */
export function thumbOutcomeText(path, c) {
  const parts = [`${plural(c.stored, 'thumbnail')} made in ${path}.`]
  if (c.skipped) parts.push(`${plural(c.skipped, 'file')} could not be drawn and kept ${c.skipped === 1 ? 'its' : 'their'} icon.`)
  if (c.failed) parts.push(`${plural(c.failed, 'file')} could not be read from ${c.failed === 1 ? 'its' : 'their'} clouds.`)
  // Drawn here but turned down by the server, which re-reads every picture.
  const refused = c.error ? 0 : c.made - c.stored
  if (refused > 0) parts.push(`${plural(refused, 'picture')} drawn here could not be stored.`)
  if (c.error) parts.push(`It stopped early: ${c.error}.`)
  else if (c.stopped) parts.push(`Stopped with ${plural(c.total - c.done, 'file')} still to do — run it again to finish.`)
  return parts.join(' ')
}

const lead = {
  margin: '0 0 14px', fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.6, color: COLORS.textDim,
}
const note = {
  margin: '0 0 16px', fontFamily: FONT.sans, fontSize: '11.5px', lineHeight: 1.6, color: COLORS.textMuted,
}
