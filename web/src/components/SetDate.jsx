import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatDate } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* A date and time somebody types in, for one file or for a whole folder.

   Fix timestamps reads a date out of a name or out of a photo, and sometimes
   what it reads is wrong or there is nothing to read: a scan of a letter, a
   camera whose clock was never set, a folder of pictures somebody knows were
   all taken at one wedding. The person looking at the folder is then the only
   source there is, and this is where they say it.

   It is the same act either way — a modified date written into the encrypted
   index, over the endpoint Fix timestamps applies its own plan through — so
   nothing is transferred and no account is contacted. For a folder, every file
   in it is given the one date, and the dialog counts them first. */

/* How many files go in one request; an index write whatever its size. */
const PER_REQUEST = 200

/* An instant as a datetime-local field wants it: this browser's wall clock,
   to the minute. */
function toField(iso) {
  const d = iso ? new Date(iso) : new Date()
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 1900) return toField(null)
  const pad = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export default function SetDateDialog({ kind, file, name, path, vault = '', onClose, onDone }) {
  const folder = kind === 'folder'
  const [value, setValue] = useState(() => toField(folder ? null : file.modified_at))
  const [deep, setDeep] = useState(false)
  const [survey, setSurvey] = useState(null)
  const [busy, setBusy] = useState(null)
  const [error, setError] = useState(null)
  const [done, setDone] = useState(null)

  /* A folder is counted before there is a button, like everything else that
     acts on files nobody picked one by one. */
  useEffect(() => {
    if (!folder) return undefined
    let live = true
    api.survey(path, vault)
      .then((resp) => { if (live) setSurvey(resp) })
      .catch((err) => { if (live) setError(err.message) })
    return () => { live = false }
  }, [folder, path, vault])

  const all = survey?.files || []
  const here = all.filter((f) => f.dir === survey?.path)
  const chosen = folder ? (deep ? all : here) : [file]

  const when = value ? new Date(value) : null
  const valid = when && !Number.isNaN(when.getTime())
  const ready = valid && chosen.length > 0 && !busy && (!folder || survey)

  const submit = async (e) => {
    e.preventDefault()
    if (!ready) return

    const modified = when.toISOString()
    setError(null)
    let fixed = 0
    try {
      for (let i = 0; i < chosen.length; i += PER_REQUEST) {
        setBusy({ at: i, total: chosen.length })
        const resp = await api.fixTimes({
          set: chosen.slice(i, i + PER_REQUEST).map((f) => ({ id: f.id, modified })),
        })
        fixed += resp?.fixed || 0
      }
      onDone?.()
      if (!folder) {
        onClose()
        return
      }
      setDone({ fixed, total: chosen.length, modified })
    } catch (err) {
      // What landed before the failure stays; saying how much is the point.
      setError(fixed > 0 ? `${err.message} — ${fixed} had been set before that.` : err.message)
      if (fixed > 0) onDone?.()
    }
    setBusy(null)
  }

  const title = folder ? 'Set a date for every file' : 'Set date and time'
  const subtitle = folder ? (path === '/' ? 'The root of the vault' : path) : (name || file.name)

  if (done) {
    return (
      <Modal title={title} subtitle={subtitle} onClose={onClose} width={440}>
        <Banner tone="success">
          {done.total} file{done.total === 1 ? '' : 's'} now dated {formatDate(done.modified)}
          {done.fixed < done.total ? ` — ${done.total - done.fixed} already ${done.total - done.fixed === 1 ? 'was' : 'were'}.` : '.'}
        </Banner>
        <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button variant="primary" onClick={onClose}>Done</Button>
        </div>
      </Modal>
    )
  }

  const option = (on, label, count) => (
    <button
      type="button"
      onClick={() => setDeep(on)}
      aria-pressed={deep === on}
      style={{
        flex: 1, minHeight: '38px', padding: '6px 10px',
        background: deep === on ? COLORS.surfaceRaised : COLORS.bg,
        border: `1px solid ${deep === on ? COLORS.accent : COLORS.border}`,
        borderRadius: '6px',
        color: deep === on ? COLORS.text : COLORS.textDim,
        fontFamily: FONT.mono, fontSize: '11.5px', cursor: 'pointer',
      }}
    >{`${label} · ${count}`}</button>
  )

  return (
    <Modal title={title} subtitle={subtitle} onClose={busy ? undefined : onClose} width={440}>
      <form onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}

        <p style={{
          margin: '0 0 12px',
          fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textMuted, lineHeight: 1.6,
        }}>
          {folder
            ? 'Every file chosen below is given this one date and time, in place of whatever it carries now. Nothing is transferred: a date is a field in the encrypted index.'
            : `Dated ${formatDate(file.modified_at)} now. Nothing is transferred: a date is a field in the encrypted index, and the file stays as it is.`}
        </p>

        {folder && !survey && !error && (
          <div style={{ padding: '18px', textAlign: 'center' }}><Spinner size={16} /></div>
        )}
        {folder && survey && (
          <div style={{ display: 'flex', gap: '6px', marginBottom: '12px' }}>
            {option(false, 'This folder', here.length)}
            {option(true, 'Everything under it', all.length)}
          </div>
        )}

        <input
          type="datetime-local"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          aria-label="Date and time"
          required
          style={{
            width: '100%', padding: '10px 12px',
            background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
            color: COLORS.text, fontFamily: FONT.mono, fontSize: '13px',
            outline: 'none', boxSizing: 'border-box', colorScheme: 'dark',
          }}
        />

        <div style={{
          minHeight: '18px', margin: '6px 0 14px',
          fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.textMuted,
        }}>
          {busy
            ? `Setting ${Math.min(busy.total, busy.at + PER_REQUEST)} of ${busy.total}…`
            : 'In this browser’s time zone.'}
        </div>

        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
          <Button type="button" variant="ghost" onClick={onClose} disabled={!!busy}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={!ready}>
            {busy ? <Spinner size={10} color={COLORS.bg} /> : null}
            {folder ? `Set the date of ${chosen.length} file${chosen.length === 1 ? '' : 's'}` : 'Set date'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}
