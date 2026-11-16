import { test } from 'node:test'
import assert from 'node:assert/strict'
import { planBackfill, runBackfill } from './thumbBackfill.js'

const imagesOnly = (mime, name) => (mime.startsWith('image/') || /\.heic$/i.test(name) ? 'image' : null)

const file = (id, extra = {}) => ({ id, name: `${id}.jpg`, mime: 'image/jpeg', size: 10, ...extra })

test('the plan is the pictureable files the listing says have none', () => {
  const listing = {
    files: [
      file('has'),
      file('needs'),
      file('song', { name: 'song.mp3', mime: 'audio/mpeg' }),
      file('phone', { name: 'IMG_1.HEIC', mime: '' }),
      file('huge', { size: 1000 }),
    ],
    thumbs: ['has'],
  }
  const { files, tooLarge } = planBackfill(listing, imagesOnly, 100)
  assert.deepEqual(files.map((f) => f.id), ['needs', 'phone'])
  assert.equal(tooLarge, 1)
})

test('an empty or missing listing plans nothing', () => {
  assert.deepEqual(planBackfill(null, imagesOnly), { files: [], tooLarge: 0 })
  assert.deepEqual(planBackfill({ files: [] }, imagesOnly), { files: [], tooLarge: 0 })
})

test('every file is made once and stored in batches no larger than asked', async () => {
  const files = ['a', 'b', 'c', 'd', 'e'].map((id) => file(id))
  const made = []
  const batches = []
  const result = await runBackfill({
    files,
    make: async (f) => { made.push(f.id); return `pic-${f.id}` },
    store: async (pictures) => { batches.push([...pictures.keys()]); return { stored: pictures.size } },
    batch: 2,
    lanes: 2,
  })

  assert.deepEqual([...made].sort(), ['a', 'b', 'c', 'd', 'e'])
  assert.ok(batches.every((b) => b.length <= 2), `batches ${JSON.stringify(batches)}`)
  assert.deepEqual(batches.flat().sort(), ['a', 'b', 'c', 'd', 'e'])
  assert.equal(result.total, 5)
  assert.equal(result.done, 5)
  assert.equal(result.made, 5)
  assert.equal(result.stored, 5)
  assert.equal(result.stopped, false)
  assert.equal(result.error, null)
})

test('a file with no picture is skipped and one that throws is failed', async () => {
  const result = await runBackfill({
    files: [file('ok'), file('blank'), file('broken')],
    make: async (f) => {
      if (f.id === 'blank') return null
      if (f.id === 'broken') throw new Error('download failed')
      return 'pic'
    },
    store: async (pictures) => ({ stored: pictures.size }),
  })
  assert.equal(result.done, 3)
  assert.equal(result.made, 1)
  assert.equal(result.skipped, 1)
  assert.equal(result.failed, 1)
  assert.equal(result.stored, 1)
})

test('batches never overlap', async () => {
  let inFlight = 0
  let most = 0
  const result = await runBackfill({
    files: Array.from({ length: 9 }, (_, i) => file(`f${i}`)),
    make: async (f) => f.id,
    store: async (pictures) => {
      inFlight += 1
      most = Math.max(most, inFlight)
      await new Promise((r) => setTimeout(r, 5))
      inFlight -= 1
      return { stored: pictures.size }
    },
    batch: 2,
    lanes: 3,
  })
  assert.equal(most, 1)
  assert.equal(result.stored, 9)
})

test('no more than `lanes` files are being made at once', async () => {
  let inFlight = 0
  let most = 0
  await runBackfill({
    files: Array.from({ length: 8 }, (_, i) => file(`f${i}`)),
    make: async () => {
      inFlight += 1
      most = Math.max(most, inFlight)
      await new Promise((r) => setTimeout(r, 2))
      inFlight -= 1
      return 'pic'
    },
    store: async (pictures) => ({ stored: pictures.size }),
    lanes: 3,
  })
  assert.equal(most, 3)
})

test('a failed store ends the run and keeps what went before', async () => {
  let calls = 0
  const made = []
  const result = await runBackfill({
    files: Array.from({ length: 10 }, (_, i) => file(`f${i}`)),
    make: async (f) => {
      made.push(f.id)
      await new Promise((r) => setTimeout(r, 1))
      return 'pic'
    },
    store: async (pictures) => {
      calls += 1
      if (calls === 2) throw new Error('vault is locked')
      return { stored: pictures.size }
    },
    batch: 2,
    lanes: 1,
  })
  assert.equal(result.error, 'vault is locked')
  assert.equal(result.stored, 2)
  assert.ok(made.length < 10, `kept fetching after the store failed: ${made.length}`)
})

test('stopping takes no new files and still stores what was drawn', async () => {
  const controller = new AbortController()
  const made = []
  const stored = []
  const result = await runBackfill({
    files: Array.from({ length: 10 }, (_, i) => file(`f${i}`)),
    make: async (f) => {
      made.push(f.id)
      if (made.length === 3) controller.abort()
      return 'pic'
    },
    store: async (pictures) => { stored.push(...pictures.keys()); return { stored: pictures.size } },
    signal: controller.signal,
    batch: 32,
    lanes: 1,
  })
  assert.equal(result.stopped, true)
  assert.deepEqual(made, ['f0', 'f1', 'f2'])
  // The third was let go by the stop; the two before it were drawn and kept.
  assert.deepEqual(stored, ['f0', 'f1'])
  assert.equal(result.done, 2)
  assert.equal(result.failed, 0)
})

test('the store is told how many it kept, and that is what is counted', async () => {
  const result = await runBackfill({
    files: [file('a'), file('b')],
    make: async () => 'pic',
    store: async () => ({ stored: 1 }),
  })
  assert.equal(result.made, 2)
  assert.equal(result.stored, 1)
})

test('progress is reported as it goes', async () => {
  const seen = []
  await runBackfill({
    files: [file('a'), file('b')],
    make: async () => 'pic',
    store: async (pictures) => ({ stored: pictures.size }),
    onProgress: (c) => seen.push(c.done),
    lanes: 1,
  })
  assert.deepEqual(seen.slice(0, 2), [1, 2])
})

test('nothing to do resolves at once', async () => {
  const result = await runBackfill({
    files: [],
    make: async () => { throw new Error('not called') },
    store: async () => { throw new Error('not called') },
  })
  assert.equal(result.total, 0)
  assert.equal(result.error, null)
})
