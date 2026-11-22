import { useEffect, useRef, useState } from 'react'
import { api } from '../api'
import { useMkvConvert } from '../hooks'
import { claimRun } from '../runClaims'
import { COLORS, FONT, formatBytes } from '../theme'
import { Banner, Button, LoadProgress, Modal, Spinner } from './ui'

/* Converting a disc image's film to an MKV.

   The server plays the film inside a Blu-ray image as it is, and most players
   take that. Not all take everything in it: a TV's player reads fewer kinds
   of sound and subtitle out of a disc's own container than out of an MKV. So
   the server can also write the film out as an MKV beside the image — every
   track copied, none re-encoded — and that file plays anywhere.

   It is a long job and a large file, so nothing starts until it has been
   said what will happen and agreed to. After that the work is the server's:
   this dialog can be closed and opened again, and finds the run where it is. */

/* The name the server will give the new file: the image's own, as an MKV. */
export function mkvNameFor(name) {
  return name.replace(/\.iso$/i, '') + '.mkv'
}

/* A running conversion in words, for this dialog's bar and for the strip over
   the file list that carries it once the dialog is closed (BackgroundRuns). */
export function describeMkv(at, elapsed) {
  if (!at) return { label: 'Starting…', detail: elapsed, fraction: null }
  const label = at.stage === 'storing'
    ? `Storing ${at.name}…`
    : 'Reading the film out of the disc image…'
  if (!(at.total > 0)) return { label, detail: elapsed, fraction: null }
  return {
    label,
    detail: `${formatBytes(at.done)} of ${formatBytes(at.total)}` + (elapsed ? ` · ${elapsed}` : ''),
    fraction: Math.min(1, at.done / at.total),
  }
}

/* What a finished conversion came to, in a sentence. */
export function mkvOutcomeText(run) {
  if (run.error) {
    return /was stopped/i.test(run.error)
      ? 'Converting the disc image was stopped. Nothing was kept.'
      : `Converting the disc image did not finish: ${run.error}`
  }
  const r = run.result
  if (!r) return 'Converting the disc image was stopped. Nothing was kept.'
  return `${r.name} is in ${r.dir === '/' ? 'the top folder' : r.dir}, beside the disc image it was made from: `
    + `${formatBytes(r.size)}, with ${r.video} video, ${r.audio} audio and ${r.subtitles} subtitle tracks.`
}

export default function ConvertDisc({ file, zIndex, onClose, onDone }) {
  const { status, start: begin, refresh, error: startError } = useMkvConvert()
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  /* Whether the run this dialog is showing finish has already been reported
     to the page behind, which redraws the folder to show the new file. */
  const reported = useRef(null)

  const mine = !!status && status.scope === file.id
  const running = !!status && status.running
  const result = mine && !running ? status.result : null
  const failed = mine && !running ? status.error : ''

  /* While this dialog is open it is the one drawing the run, so the strip
     over the file list leaves it alone; closing the dialog hands it over. */
  const runID = status?.id
  useEffect(() => claimRun('mkv', runID), [runID])

  useEffect(() => {
    if (result && reported.current !== status.id) {
      reported.current = status.id
      onDone?.(result)
    }
  }, [result, status, onDone])

  const start = async () => {
    setBusy(true)
    setError(null)
    await begin(file.id)
    setBusy(false)
  }

  const stop = async () => {
    setBusy(true)
    try {
      await api.mkvStop()
      await refresh()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const name = mkvNameFor(file.name)
  const text = { fontFamily: FONT.sans, fontSize: '13px', lineHeight: 1.6, color: COLORS.text }
  const buttons = { display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap', marginTop: '18px' }

  let body
  if (!status) {
    body = <div style={{ padding: '32px', textAlign: 'center' }}><Spinner size={20} /></div>
  } else if (status && !status.available) {
    body = (
      <>
        <Banner tone="warn">{status.reason}</Banner>
        <p style={text}>
          Install it on that machine and try again: on Debian, Ubuntu or a
          Raspberry Pi, <code>sudo apt install ffmpeg</code>, or run the SAND
          installer again, which now installs it.
        </p>
        <div style={buttons}><Button variant="primary" onClick={onClose}>Close</Button></div>
      </>
    )
  } else if (running && mine) {
    const at = describeMkv(status.progress, '')
    body = (
      <>
        <div style={{ display: 'flex', justifyContent: 'center', padding: '18px 0 10px' }}>
          <LoadProgress width={380} label={at.label} fraction={at.fraction} detail={at.detail || null} />
        </div>
        <p style={{ ...text, color: COLORS.textDim, textAlign: 'center' }}>
          The server is doing this. You can close this and carry on: a bar
          over the file list follows it, and the new file appears in the
          folder when it is done.
        </p>
        <div style={buttons}>
          <Button variant="danger" disabled={busy} onClick={stop}>Stop</Button>
          <Button variant="primary" onClick={onClose}>Close</Button>
        </div>
      </>
    )
  } else if (running) {
    body = (
      <>
        <Banner tone="info">
          Another disc image is being converted
          {status.progress?.name ? <> (<code>{status.progress.name}</code>)</> : null}. One
          runs at a time; try this one when it has finished.
        </Banner>
        <div style={buttons}><Button variant="primary" onClick={onClose}>Close</Button></div>
      </>
    )
  } else if (result) {
    const tracks = [
      `${result.video} video`,
      `${result.audio} audio`,
      `${result.subtitles} subtitle`,
    ].join(', ')
    body = (
      <>
        <Banner tone="success">
          <code>{result.name}</code> is in the folder beside the
          image: {formatBytes(result.size)}, with {tracks} tracks.
        </Banner>
        {(result.rewritten || []).length > 0 && (
          <p style={text}>
            Stored another way rather than copied, with the same
            samples: {result.rewritten.join('; ')}.
          </p>
        )}
        {(result.dropped || []).length > 0 && (
          <Banner tone="warn">
            Left out, because ffmpeg could not tell what they
            hold: {result.dropped.join('; ')}.
          </Banner>
        )}
        {(result.warnings || []).map((w) => <Banner key={w} tone="warn">{w}</Banner>)}
        <p style={{ ...text, color: COLORS.textDim }}>
          The disc image is untouched. Play the new file to check it before
          you remove the image.
        </p>
        <div style={buttons}><Button variant="primary" onClick={onClose}>Done</Button></div>
      </>
    )
  } else {
    body = (
      <>
        {failed && <Banner tone="error">{mkvOutcomeText({ error: failed })}</Banner>}
        <p style={text}>
          This writes the main film on this disc image to a new
          file, <code>{name}</code>, beside it. The picture, every soundtrack
          and every subtitle track are copied as they are: nothing is
          re-encoded, and the tracks are labelled with their languages.
        </p>
        <ul style={{ ...text, paddingLeft: '20px', margin: '10px 0' }}>
          <li>The new file is about the size of the film, up to {formatBytes(file.size)}, and is stored like any other file.</li>
          <li>While it works, the server needs that much free disk space of its own.</li>
          <li>It takes as long as downloading and uploading the film: for a UHD disc, hours.</li>
          <li>The disc image stays as it is. The disc's menus, extras and chapters are not carried over.</li>
        </ul>
        <div style={buttons}>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={busy} onClick={start}>
            {busy ? 'Starting…' : failed ? 'Try again' : 'Convert to MKV'}
          </Button>
        </div>
      </>
    )
  }

  return (
    <Modal title="Convert to MKV" subtitle={file.name} onClose={onClose} width={540} zIndex={zIndex}>
      {(error || startError) && <Banner tone="error" onDismiss={() => setError(null)}>{error || startError}</Banner>}
      {body}
    </Modal>
  )
}
