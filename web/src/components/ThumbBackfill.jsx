import React, { useEffect, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { useThumbMaking } from '../hooks'
import { Banner, Button, Modal, Spinner } from './ui'

/* "Make missing thumbnails", from the organizer.

   The run is a job on the server, like every other long job: the server reads
   each file back from its clouds, draws it and stores its thumbnail, so the
   page that started it can be closed, the phone it was on can sleep, and the
   vault can even be locked — the run waits and carries on after the unlock.
   Nothing but Stop ends it, and it says at the end how many failed and why.

   The dialog counts what a run would do, for this folder and every folder
   below it, before offering to do it, as every organizer tool does. Starting
   closes it; the strip above the file list (BackgroundRuns) carries the run
   from then on. */

const plural = (n, one, many = `${one}s`) => `${n.toLocaleString()} ${n === 1 ? one : many}`

const MAX_SOURCE_MB = 80

export function MakeThumbnails({ path, vault = '', onClose }) {
  const { status, start, error: startError } = useThumbMaking()
  const [plan, setPlan] = useState(null)
  const [error, setError] = useState(null)

  useEffect(() => {
    let live = true
    api.thumbPlan(path, vault)
      .then((p) => { if (live) setPlan(p) })
      .catch((err) => { if (live) setError(err.message) })
    return () => { live = false }
  }, [path, vault])

  const begin = async () => {
    if (await start(path, vault)) onClose()
  }

  const busy = !!status?.running
  const here = path === '/' ? 'the top of the vault' : path
  const busyWith = status?.progress?.path || ''

  return (
    <Modal title="Make missing thumbnails" subtitle={path} onClose={onClose} width={460}>
      {(error || startError) && <Banner tone="error">{error || startError}</Banner>}

      {!error && !plan && (
        <div style={{ padding: '28px', textAlign: 'center' }}><Spinner size={18} /></div>
      )}

      {plan && (
        <>
          <p style={lead}>
            Every picture and PDF in {here}, and in every folder below it, that has no
            thumbnail is read back from its clouds and drawn small on the server, and its
            thumbnail stored.
          </p>

          {busy ? (
            <Banner tone="info">
              Thumbnails are already being made for {busyWith === path ? 'this folder' : busyWith || 'another folder'}.
              {' '}It shows above the file list, where it can be stopped; start this once it is done.
            </Banner>
          ) : plan.files === 0 ? (
            <Banner tone="success">
              Nothing to make — every picture and PDF here and below already has its thumbnail.
            </Banner>
          ) : (
            <Banner tone="info">
              {plural(plan.files, 'file')}
              {plan.folders > 1 ? ` in ${plural(plan.folders, 'folder')}` : ' here'}
              {plan.files === 1 ? ' has' : ' have'} no thumbnail.
            </Banner>
          )}

          {plan.too_large > 0 && (
            <p style={note}>
              {plural(plan.too_large, 'file')} over {MAX_SOURCE_MB} MB
              {plan.too_large === 1 ? ' is' : ' are'} left out: drawing a picture that large costs more than the thumbnail is worth.
            </p>
          )}

          {plan.over_full > 0 && (
            <p style={note}>
              {plural(plan.over_full, 'file')} {plan.over_full === 1 ? 'is' : 'are'} left out: a folder
              keeps at most 2,048 thumbnails, and the ones past that are the files it lists last.
            </p>
          )}

          {!busy && plan.files > 0 && (
            <p style={note}>
              It runs on the server: you can close this page, or lock the vault, and it carries
              on — after a lock, once the vault is unlocked again. It goes on to the end, and says
              then how many failed and why.
            </p>
          )}

          <div style={{ display: 'flex', gap: '10px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
            {!busy && plan.files > 0 && (
              <Button variant="primary" onClick={begin}>
                Make {plural(plan.files, 'thumbnail')}
              </Button>
            )}
            <Button variant="ghost" onClick={onClose}>{busy || plan.files === 0 ? 'Close' : 'Cancel'}</Button>
          </div>
        </>
      )}
    </Modal>
  )
}

/* The bar's sentence and how full it is, for a run going. */
export function describeThumbMaking(at, elapsed) {
  if (!at) return { label: 'Counting the files…', detail: elapsed, fraction: null }
  const counts = `${at.done.toLocaleString()} of ${at.total.toLocaleString()} · ${plural(at.made, 'thumbnail')} made`
    + (at.failed ? ` · ${at.failed.toLocaleString()} failed` : '')
  if (at.locked) {
    return {
      label: `The vault is locked — this carries on once it is unlocked · ${counts}`,
      detail: elapsed,
      fraction: at.total ? at.done / at.total : null,
    }
  }
  return {
    label: `${counts}${elapsed ? ` · ${elapsed}` : ''}`,
    detail: '',
    fraction: at.total ? at.done / at.total : null,
  }
}

/* The sentence a finished run comes to: the total first, then why. */
export function thumbOutcomeText(r) {
  if (!r) return 'Making the thumbnails did not finish.'
  const parts = [`${plural(r.made, 'thumbnail')} made in ${r.path}, ${(r.failed || 0).toLocaleString()} failed.`]
  const why = []
  if (r.unread) why.push(`${r.unread.toLocaleString()} could not be read from ${r.unread === 1 ? 'its' : 'their'} clouds`)
  if (r.undrawn) why.push(`${r.undrawn.toLocaleString()} could not be drawn`)
  if (r.unstored) why.push(`${r.unstored.toLocaleString()} could not be stored`)
  if (why.length) parts.push(`Of those, ${why.join(', ')}.`)
  if (r.errors?.length) {
    const first = r.errors[0]
    parts.push(`For example ${first.path}: ${first.error}.`)
  }
  if (r.stopped) parts.push(`Stopped with ${plural(r.total - r.done, 'file')} still to do — run it again to finish.`)
  else if (r.failed > 0) parts.push('Running it again tries the failed ones once more.')
  return parts.join(' ')
}

const lead = {
  margin: '0 0 14px', fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.6, color: COLORS.textDim,
}
const note = {
  margin: '0 0 16px', fontFamily: FONT.sans, fontSize: '11.5px', lineHeight: 1.6, color: COLORS.textMuted,
}
