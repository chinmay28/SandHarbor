import { test } from 'node:test'
import assert from 'node:assert/strict'
import { backoffDelay, classifyError, planBackfill, runBackfill } from './thumbBackfill.js'

const imagesOnly = (mime, name) => (mime.startsWith('image/') || /\.heic$/i.test(name) ? 'image' : null)

const file = (id, extra = {}) => ({ id, name: `${id}.jpg`, mime: 'image/jpeg', size: 10, ...extra })

test('the plan is the pictureable files of every folder, within each one\'s room', () => {
  const missing = {
    folders: [
      {
        dir: '/photos',
        room: 10,
        files: [
          file('needs'),
          file('song', { name: 'song.mp3', mime: 'audio/mpeg' }),
          file('phone', { name: 'IMG_1.HEIC', mime: '' }),
          file('huge', { size: 1000 }),
        ],
      },
      { dir: '/photos/aug', room: 2, files: [file('a1'), file('a2'), file('a3')] },
      { dir: '/photos/full', room: 0, files: [file('f1')] },
      { dir: '/photos/music', room: 5, files: [file('tune', { name: 'tune.mp3', mime: 'audio/mpeg' })] },
    ],
  }
  const plan = planBackfill(missing, imagesOnly, 100)
  assert.deepEqual(plan.files.map((f) => f.id), ['needs', 'phone', 'a1', 'a2'])
  assert.equal(plan.folders, 2)
  assert.equal(plan.tooLarge, 1)
  assert.equal(plan.overFull, 2)
})

test('a file that is not pictureable or too large takes none of the room', () => {
  const missing = {
    folders: [{
      dir: '/', room: 1,
      files: [file('song', { name: 'song.mp3', mime: 'audio/mpeg' }), file('huge', { size: 1000 }), file('ok')],
    }],
  }
  assert.deepEqual(planBackfill(missing, imagesOnly, 100).files.map((f) => f.id), ['ok'])
})

test('an empty or missing answer plans nothing', () => {
  const none = { files: [], folders: 0, tooLarge: 0, overFull: 0 }
  assert.deepEqual(planBackfill(null, imagesOnly), none)
  assert.deepEqual(planBackfill({ folders: [] }, imagesOnly), none)
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

const offline = () => Object.assign(new Error('Failed to fetch'), { offline: true })
const status = (code, message = `status ${code}`) => Object.assign(new Error(message), { status: code })
const noWait = async () => {}

test('errors are read as offline, worth retrying, ending the run, or one failure', () => {
  assert.equal(classifyError(offline()), 'offline')
  assert.equal(classifyError(status(503)), 'retry')
  assert.equal(classifyError(status(429)), 'retry')
  assert.equal(classifyError(status(401)), 'end')
  assert.equal(classifyError(status(404)), 'fail')
  assert.equal(classifyError(new Error('cannot decode')), 'fail')
})

test('the backoff doubles and is capped', () => {
  assert.deepEqual([1, 2, 3, 4].map(backoffDelay), [1000, 2000, 4000, 8000])
  assert.equal(backoffDelay(20), 30000)
})

test('losing the connection waits and carries on, failing nothing', async () => {
  // Every file's first fetch fails as a backgrounded tab's does.
  const seen = new Map()
  const waits = []
  const result = await runBackfill({
    files: Array.from({ length: 6 }, (_, i) => file(`f${i}`)),
    make: async (f) => {
      const n = (seen.get(f.id) || 0) + 1
      seen.set(f.id, n)
      if (n < 3) throw offline()
      return 'pic'
    },
    store: async (pictures) => ({ stored: pictures.size }),
    onProgress: (c) => waits.push(c.waiting),
    pause: noWait,
    batch: 4,
  })
  assert.equal(result.failed, 0)
  assert.equal(result.made, 6)
  assert.equal(result.stored, 6)
  assert.equal(result.error, null)
  assert.equal(result.waiting, false)
  assert.ok(waits.includes(true), 'never said it was waiting')
})

test('an offline wait is not given up after the usual tries', async () => {
  let calls = 0
  const result = await runBackfill({
    files: [file('a')],
    make: async () => { calls += 1; if (calls < 20) throw offline(); return 'pic' },
    store: async (pictures) => ({ stored: pictures.size }),
    pause: noWait,
    tries: 2,
  })
  assert.equal(calls, 20)
  assert.equal(result.stored, 1)
})

test('a store that cannot reach the server waits and is sent again', async () => {
  let calls = 0
  const result = await runBackfill({
    files: Array.from({ length: 4 }, (_, i) => file(`f${i}`)),
    make: async () => 'pic',
    store: async (pictures) => {
      calls += 1
      if (calls <= 3) throw offline()
      return { stored: pictures.size }
    },
    pause: noWait,
    batch: 2,
    lanes: 1,
  })
  assert.equal(result.stored, 4)
  assert.equal(result.error, null)
})

test('a busy server is tried a few times, then that file is failed and the rest go on', async () => {
  const attempts = new Map()
  const result = await runBackfill({
    files: [file('busy'), file('ok')],
    make: async (f) => {
      attempts.set(f.id, (attempts.get(f.id) || 0) + 1)
      if (f.id === 'busy') throw status(503)
      return 'pic'
    },
    store: async (pictures) => ({ stored: pictures.size }),
    pause: noWait,
    tries: 3,
    lanes: 1,
  })
  assert.equal(attempts.get('busy'), 3)
  assert.equal(result.failed, 1)
  assert.equal(result.stored, 1)
  assert.equal(result.error, null)
})

test('a batch turned down is let go and the run goes on', async () => {
  let calls = 0
  const result = await runBackfill({
    files: Array.from({ length: 6 }, (_, i) => file(`f${i}`)),
    make: async () => 'pic',
    store: async (pictures) => {
      calls += 1
      if (calls === 1) throw status(400, 'bad batch')
      return { stored: pictures.size }
    },
    pause: noWait,
    batch: 2,
    lanes: 1,
  })
  assert.equal(result.error, null)
  assert.equal(result.made, 6)
  assert.equal(result.stored, 4)
})

test('several batches turned down in a row end the run', async () => {
  const result = await runBackfill({
    files: Array.from({ length: 20 }, (_, i) => file(`f${i}`)),
    make: async () => { await new Promise((r) => setTimeout(r, 1)); return 'pic' },
    store: async () => { throw status(400, 'refused') },
    pause: noWait,
    batch: 2,
    lanes: 1,
    storeFails: 3,
  })
  assert.equal(result.error, 'refused')
  assert.ok(result.done < 20, `kept going after the store kept refusing: ${result.done}`)
})

test('a locked vault while reading ends the run', async () => {
  const made = []
  const result = await runBackfill({
    files: Array.from({ length: 10 }, (_, i) => file(`f${i}`)),
    make: async (f) => {
      made.push(f.id)
      if (made.length === 3) throw status(401, 'vault is locked')
      return 'pic'
    },
    store: async (pictures) => ({ stored: pictures.size }),
    pause: noWait,
    lanes: 1,
  })
  assert.equal(result.error, 'vault is locked')
  assert.equal(made.length, 3)
  // Drawn, but a locked vault would turn the batch down too, so none is sent.
  assert.equal(result.made, 2)
  assert.equal(result.stored, 0)
  assert.equal(result.failed, 0)
})

test('stopping ends an offline wait', async () => {
  const controller = new AbortController()
  const result = await runBackfill({
    files: [file('a'), file('b')],
    make: async () => { throw offline() },
    store: async (pictures) => ({ stored: pictures.size }),
    pause: async (_, signal) => {
      setTimeout(() => controller.abort(), 1)
      await new Promise((r) => signal.addEventListener('abort', r))
    },
    signal: controller.signal,
    lanes: 1,
  })
  assert.equal(result.stopped, true)
  assert.equal(result.failed, 0)
  assert.equal(result.done, 0)
  assert.equal(result.waiting, false)
})

test('a locked vault while storing ends the run and keeps what went before', async () => {
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
      if (calls === 2) throw status(401, 'vault is locked')
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
