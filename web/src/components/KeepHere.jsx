import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal, Spinner } from './ui'
import { READ_SOURCES, ReadFromChoice } from './Replication'

/* Keeping files on the machine running SAND.

   A file or folder kept here has enough of its sealed parts on that machine's
   own disk to open, stream and seek without waiting on the clouds. What is
   kept is the parts exactly as the clouds hold them, encrypted; a file is put
   back together only when it is opened. Everything kept has an end date, and
   the cache never takes the disk below its reserve.

   "This machine" is the one running SAND, not necessarily the one holding the
   browser: a laptop looking at a vault on a home server keeps its films on the
   server, which is where the streams come from. */

const CHOICES = [
  { key: '1d', label: 'A day' },
  { key: '7d', label: 'A week' },
  { key: '30d', label: 'A month' },
  { key: '90d', label: 'Three months' },
  { key: '365d', label: 'A year' },
  { key: 'date', label: 'Until a date…' },
]

const label = {
  fontFamily: FONT.mono, fontSize: '10px', fontWeight: 600, letterSpacing: '1.5px',
  textTransform: 'uppercase', color: COLORS.textMuted, margin: '16px 0 8px',
}

const prose = { fontFamily: FONT.sans, fontSize: '12.5px', color: COLORS.textDim, lineHeight: 1.6 }

export function formatUntil(iso) {
  const d = new Date(iso)
  return d.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' })
}

/* The pin naming this path, if any, out of a cache status. */
export function pinFor(status, path) {
  return status?.pins?.find((p) => p.path === path) || null
}

function pinProgress(pin) {
  if (!pin) return ''
  if (!pin.found) return 'no longer in the vault'
  if (pin.complete) return `all ${formatBytes(pin.bytes)} here`
  return `${formatBytes(pin.cached)} of ${formatBytes(pin.bytes)} here`
}

/* Which placement a folder keeps, chosen the way a stream link's is. A file
   uses ReadFromChoice itself, which can draw where its two placements are; a
   folder has many files and no one set of clouds to draw. */
function FolderSourceChoice({ value, onChange }) {
  return (
    <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap' }} role="radiogroup" aria-label="Keep from">
      {READ_SOURCES.map((s) => (
        <Button
          key={s.id}
          type="button"
          size="sm"
          role="radio"
          aria-checked={value === s.id}
          title={s.hint}
          variant={value === s.id ? 'primary' : 'default'}
          onClick={() => onChange(s.id)}
        >{s.label}</Button>
      ))}
    </div>
  )
}

const SOURCE_NOTE = {
  parts: 'Its parts are kept, as every read uses them.',
  replica: 'Its replica is kept, alone: the copy on the replication clouds. Choose Replica when playing it too, unless it is cut like the parts.',
  either: 'Its parts are kept, and its replica only for what the parts cannot give.',
}

/* The dialog a file's or folder's menu opens. file is the file's row, for a
   file; replicas says whether anything under a folder is replicated. The
   placement is offered only where there is a replica to choose. */
export function KeepHereDialog({ path, name, folder, file, replicas = false, zIndex, onClose, onChanged }) {
  const [status, setStatus] = useState(null)
  const [choice, setChoice] = useState('7d')
  const [source, setSource] = useState(null)
  const [date, setDate] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  useEffect(() => {
    let live = true
    api.cacheStatus().then((s) => { if (live) setStatus(s) }).catch((err) => { if (live) setError(err.message) })
    return () => { live = false }
  }, [])

  const pin = pinFor(status, path)
  const room = status?.usage?.room ?? 0
  const canChoose = folder ? replicas : !!file?.replica
  const from = source || pin?.from || 'parts'

  const keep = async () => {
    setBusy(true)
    setError(null)
    try {
      if (choice === 'date') {
        if (!date) throw new Error('Pick the date to keep it until')
        // The whole of the day picked, in this browser's time.
        const until = new Date(`${date}T00:00:00`)
        until.setDate(until.getDate() + 1)
        await api.keepLocally(path, { until: until.toISOString(), from })
      } else {
        await api.keepLocally(path, { keepFor: choice, from })
      }
      onChanged?.()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  const stop = async () => {
    setBusy(true)
    setError(null)
    try {
      await api.stopKeeping(pin.id)
      onChanged?.()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title={pin ? `Kept on this machine` : `Keep ${folder ? 'this folder' : 'this file'} on this machine`}
      subtitle={path}
      onClose={busy ? undefined : onClose}
      width={480}
      zIndex={zIndex}
    >
      {error && <Banner tone="error">{error}</Banner>}
      <div style={prose}>
        {pin ? (
          <p style={{ margin: 0 }}>
            Kept from its {pin.from || 'parts'} until <strong style={{ color: COLORS.text }}>{formatUntil(pin.until)}</strong>
            {' '}· {pinProgress(pin)}. Pick a new length below to change the date.
          </p>
        ) : (
          <p style={{ margin: 0 }}>
            {name} opens, streams and seeks from the disk of the machine running SAND instead of
            waiting on the clouds. What is kept is its shards, still encrypted, and only the
            fewest that rebuild it; it is put back together only when it is opened.
            {folder ? ' Files added to the folder later are kept too.' : ''}
          </p>
        )}

        {canChoose && (
          <>
            {/* The file's picker is the stream dialog's own, label and all,
                so the choice reads the same in both places. */}
            {folder ? (
              <>
                <div style={label}>Read from</div>
                <FolderSourceChoice value={from} onChange={setSource} />
              </>
            ) : (
              <div style={{ marginTop: '16px' }}>
                <ReadFromChoice file={file} value={from} onChange={setSource} />
              </div>
            )}
            <p style={{ margin: '8px 0 0', fontSize: '11.5px', color: COLORS.textMuted }}>
              {SOURCE_NOTE[from]}
              {folder && from === 'replica' ? ' Files under it with no replica are left out.' : ''}
            </p>
          </>
        )}

        <div style={label}>Keep it for</div>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px' }}>
          {CHOICES.map((c) => (
            <Button
              key={c.key}
              type="button"
              size="sm"
              variant={choice === c.key ? 'primary' : 'default'}
              onClick={() => setChoice(c.key)}
              disabled={busy}
            >{c.label}</Button>
          ))}
        </div>
        {choice === 'date' && (
          <div style={{ marginTop: '10px' }}>
            <Input
              type="date"
              label="Until the end of"
              value={date}
              min={new Date().toISOString().slice(0, 10)}
              onChange={(e) => setDate(e.target.value)}
            />
          </div>
        )}

        {status && (
          <p style={{ margin: '14px 0 0', fontSize: '11.5px', color: COLORS.textMuted }}>
            {formatBytes(status.usage?.used || 0)} kept here now; room for {formatBytes(room)} more.
            What does not fit stays on the clouds, and nothing already kept is pushed out for it.
          </p>
        )}
      </div>

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap', marginTop: '18px' }}>
        {pin && (
          <Button type="button" variant="danger" onClick={stop} disabled={busy}>Stop keeping</Button>
        )}
        <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
        <Button type="button" variant="primary" onClick={keep} disabled={busy || !status}>
          {busy ? <Spinner size={10} /> : null}{pin ? 'Save' : '⤓ Keep here'}
        </Button>
      </div>
    </Modal>
  )
}

/* The status line for Vault settings. */
export function cacheStatusLine(status) {
  if (!status) return null
  const n = status.pins?.length || 0
  if (n === 0) return 'Nothing kept'
  return `${n} kept · ${formatBytes(status.usage?.used || 0)}`
}

const GB = 1 << 30

/* The settings panel: everything kept, how far along it is, and the room. */
export function LocalCacheSettings({ zIndex, onClose }) {
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const [limit, setLimit] = useState('')
  const [reserve, setReserve] = useState('')

  const load = () => api.cacheStatus().then((s) => {
    setStatus(s)
    setLimit(s.limit > 0 ? String(Math.round((s.limit / GB) * 10) / 10) : '')
    setReserve(s.reserve > 0 ? String(Math.round((s.reserve / GB) * 10) / 10) : s.reserve < 0 ? '0' : '')
  }).catch((err) => setError(err.message))

  useEffect(() => {
    load()
    // Filling runs on the server; look again now and then while it does.
    const t = setInterval(() => { api.cacheStatus().then(setStatus).catch(() => {}) }, 5000)
    return () => clearInterval(t)
  }, [])

  const stop = async (id) => {
    setBusy(true)
    setError(null)
    try {
      await api.stopKeeping(id)
      await load()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const saveLimits = async () => {
    setBusy(true)
    setError(null)
    try {
      const toBytes = (text) => Math.round(parseFloat(text) * GB)
      const l = limit.trim() === '' ? 0 : toBytes(limit)
      // Empty is the default reserve; 0 is none at all.
      const r = reserve.trim() === '' ? 0 : (parseFloat(reserve) === 0 ? -1 : toBytes(reserve))
      if (Number.isNaN(l) || Number.isNaN(r) || l < 0) throw new Error('Sizes are numbers of GB')
      const s = await api.setCacheLimits({ limit: l, reserve: r })
      setStatus(s)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const u = status?.usage
  return (
    <Modal
      title="Kept on this machine"
      subtitle="Files and folders whose sealed parts are on this machine's disk, so they open without the clouds"
      onClose={onClose}
      width={560}
      zIndex={zIndex}
    >
      {error && <Banner tone="error">{error}</Banner>}
      {status?.problem && <Banner tone="warn">{status.problem}</Banner>}
      {!status && !error && <Spinner />}
      {status && (
        <div style={prose}>
          <p style={{ margin: 0 }}>
            {formatBytes(u?.used || 0)} kept{u?.limit > 0 ? ` of at most ${formatBytes(u.limit)}` : ''},
            room for {formatBytes(u?.room || 0)} more
            {u?.disk_total > 0
              ? (u.reserve > 0 ? `, leaving ${formatBytes(u.reserve)} of the disk free` : ', with no disk kept in reserve')
              : ''}.
            {status.filling ? ' Fetching now…' : ''}
          </p>

          <div style={label}>What is kept</div>
          {status.pins.length === 0 && (
            <p style={{ margin: 0 }}>Nothing yet. A file's or folder's menu has <em>Keep on this machine</em>.</p>
          )}
          <div style={{ display: 'flex', flexDirection: 'column', gap: '6px' }}>
            {status.pins.map((p) => {
              const fraction = p.bytes > 0 ? Math.min(p.cached / p.bytes, 1) : (p.complete ? 1 : 0)
              return (
                <div key={p.id} style={{
                  display: 'flex', alignItems: 'center', gap: '10px', padding: '8px 10px',
                  background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
                }}>
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <div style={{
                      fontFamily: FONT.mono, fontSize: '12px', color: COLORS.text,
                      overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                    }}>{p.folder ? '📁 ' : ''}{p.path}</div>
                    <div style={{ fontSize: '11px', color: COLORS.textMuted }}>
                      {p.from && p.from !== 'parts' ? `from ${p.from} · ` : ''}until {formatUntil(p.until)} · {p.files} file{p.files === 1 ? '' : 's'} · {pinProgress(p)}
                      {p.unsupported > 0 ? ` · ${p.unsupported} to convert first` : ''}
                      {p.no_replica > 0 ? ` · ${p.no_replica} with no replica` : ''}
                    </div>
                    <div style={{ height: '3px', background: COLORS.border, borderRadius: '2px', marginTop: '5px' }}>
                      <div style={{
                        width: `${fraction * 100}%`, height: '100%', borderRadius: '2px',
                        background: p.complete ? COLORS.success || COLORS.accent : COLORS.accent,
                      }} />
                    </div>
                  </div>
                  <Button type="button" size="sm" variant="ghost" disabled={busy} onClick={() => stop(p.id)}>
                    Stop
                  </Button>
                </div>
              )
            })}
          </div>

          <div style={label}>Room</div>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: '10px' }}>
            <Input
              label="Most it may hold (GB)"
              help="Empty for no limit of its own"
              inputMode="decimal"
              value={limit}
              onChange={(e) => setLimit(e.target.value)}
            />
            <Input
              label="Disk to leave free (GB)"
              help="Empty for a tenth of the disk; 0 for none"
              inputMode="decimal"
              value={reserve}
              onChange={(e) => setReserve(e.target.value)}
            />
          </div>
        </div>
      )}
      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '18px' }}>
        <Button type="button" variant="ghost" onClick={onClose}>Close</Button>
        <Button type="button" variant="primary" onClick={saveLimits} disabled={busy || !status}>Save room</Button>
      </div>
    </Modal>
  )
}
