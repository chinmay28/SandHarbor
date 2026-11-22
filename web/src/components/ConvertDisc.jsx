import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
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

const POLL_MS = 1000

/* The name the server will give the new file: the image's own, as an MKV. */
export function mkvNameFor(name) {
  return name.replace(/\.iso$/i, '') + '.mkv'
}

export default function ConvertDisc({ file, zIndex, onClose, onDone }) {
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  /* Whether the run this dialog is showing finish has already been reported
     to the page behind, which redraws the folder to show the new file. */
  const reported = useRef(null)

  const poll = useCallback(() => api.mkvStatus()
    .then((s) => { setStatus(s); return s })
    .catch((err) => { setError(err.message); return null }), [])

  useEffect(() => { poll() }, [poll])

  const mine = !!status && status.scope === file.id
  const running = !!status && status.running
  const result = mine && !running ? status.result : null
  const failed = mine && !running ? status.error : ''

  // Followed for as long as something is running, whoever started it.
  useEffect(() => {
    if (!running) return undefined
    const timer = window.setInterval(poll, POLL_MS)
    return () => window.clearInterval(timer)
  }, [running, poll])

  useEffect(() => {
    if (result && reported.current !== status.id) {
      reported.current = status.id
      onDone?.(result)
    }
  }, [result, status, onDone])

  const start = async () => {
    setBusy(true)
    setError(null)
    try {
      await api.mkvStart(file.id)
      await poll()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const stop = async () => {
    setBusy(true)
    try {
      setStatus(await api.mkvStop())
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
  if (!status && !error) {
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
    const p = status.progress || {}
    const reading = p.stage !== 'storing'
    body = (
      <>
        <div style={{ display: 'flex', justifyContent: 'center', padding: '18px 0 10px' }}>
          <LoadProgress
            width={380}
            label={reading ? 'Reading the film out of the disc image…' : `Storing ${name}…`}
            fraction={p.total > 0 ? p.done / p.total : null}
            detail={p.total > 0 ? `${formatBytes(p.done)} of ${formatBytes(p.total)}` : null}
          />
        </div>
        <p style={{ ...text, color: COLORS.textDim, textAlign: 'center' }}>
          The server is doing this. You can close this and carry on; the new
          file appears in the folder when it is done.
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
        {failed && <Banner tone="error">The last try failed: {failed}</Banner>}
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
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}
      {body}
    </Modal>
  )
}
