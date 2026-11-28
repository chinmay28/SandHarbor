import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  archiveFolder, archivePicks, describeExtract, extractFraction, extractNotes, extractOutcomeText,
  extractProgressText, goesInPieces, isArchive, sendInPieces, BATCH_BYTES,
} from './upload.js'

const pick = (name) => ({ file: { name }, path: name })

test('an archive is known by its ending, whatever its case', () => {
  for (const name of ['photos.zip', 'PHOTOS.ZIP', 'src.tar.gz', 'src.tgz', 'a.b.Tar.Gz']) {
    assert.equal(isArchive(name), true, name)
  }
})

test('anything else is a file like any other', () => {
  for (const name of ['notes.txt', 'backup.tar', 'report.gz', 'zip', '.zip', 'a.zip.part', '']) {
    assert.equal(isArchive(name), false, name)
  }
})

test('the folder is the name without the whole ending', () => {
  assert.equal(archiveFolder('photos.zip'), 'photos')
  assert.equal(archiveFolder('Trip 2024.ZIP'), 'Trip 2024')
  assert.equal(archiveFolder('src-1.0.tar.gz'), 'src-1.0')
  assert.equal(archiveFolder('src.tgz'), 'src')
  assert.equal(archiveFolder('notes.txt'), 'notes.txt')
})

test('only the archives of a choice are offered', () => {
  const picks = { files: [pick('a.zip'), pick('b.txt'), pick('c.tar.gz')], dirs: [] }
  assert.deepEqual(archivePicks(picks).map(({ file }) => file.name), ['a.zip', 'c.tar.gz'])
})

test('one archive is named with the folder it becomes; several are counted', () => {
  assert.match(describeExtract([pick('trip.tar.gz')]), /Extract trip\.tar\.gz into a folder named trip,/)
  assert.match(describeExtract([pick('a.zip'), pick('b.zip')]), /the 2 archives/)
})

const run = (fields) => ({ name: 'trip.zip', dir: '/inbox/trip', ...fields })

test('a run is measured by how much of its archive has been read', () => {
  assert.equal(extractFraction(run({ at: { done: 25, total: 100 } })), 0.25)
  assert.equal(extractFraction(run({ at: { done: 150, total: 100 } })), 1)
  assert.equal(extractFraction(run({ at: {} })), null)
})

test('progress says how many are stored and which is in hand', () => {
  assert.equal(extractProgressText(run({ at: { files: 1, current: 'a/b.jpg' } })), '1 file stored · a/b.jpg')
  assert.equal(extractProgressText(run({ at: { files: 12 } })), '12 files stored')
  assert.match(extractProgressText(run({ waiting: true })), /waiting/)
})

test('an outcome always says how much is in the folder', () => {
  assert.equal(
    extractOutcomeText(run({ report: { extracted: 3 } })),
    'Extracted trip.zip into /inbox/trip — 3 files stored.')
  assert.match(extractOutcomeText(run({ report: { extracted: 3, failed: 2 } })), /3 files stored, 2 could not be\./)
  assert.match(extractOutcomeText(run({ cancelled: true, report: { extracted: 1 } })), /^Stopped.*The 1 file already stored/)
  assert.match(extractOutcomeText(run({ cancelled: true })), /Nothing had been stored/)
  assert.match(extractOutcomeText(run({ error: 'cut short', report: { extracted: 2 } })), /cut short.*2 files stored before/)
})

test('the notes name the first few and count the rest', () => {
  const notes = extractNotes(run({
    report: { failed: 12, failures: ['a: no', 'b: no'], skipped: 3, skips: ['l (a symbolic link)'], warnings: ['w'] },
  }))
  assert.deepEqual(notes, [
    'a: no', 'b: no', '…and 10 more that could not be stored.',
    'Left out, not being files: l (a symbolic link) and 2 more.', 'w',
  ])
  assert.deepEqual(extractNotes(run({ report: { extracted: 5 } })), [])
})

/* A stand-in for a File: bytes are their own offsets, so what the "server"
   was given can be checked against where it was put. */
const fakeFile = (size) => ({
  size,
  slice: (start, end) => ({ start, end, size: end - start }),
})

/* A server that keeps what it is sent, in order, and can be told to
   misbehave on a given call. */
function fakeServer(misbehave = {}) {
  const state = { received: 0, calls: 0, pieces: [] }
  const send = async (offset, blob) => {
    const call = state.calls++
    const act = misbehave[call]
    if (act === 'drop') throw new Error('network error')
    if (offset !== state.received) {
      throw Object.assign(new Error('offset'), { received: state.received })
    }
    state.received = blob.end
    state.pieces.push([blob.start, blob.end])
    // Landed, but the answer never made it back.
    if (act === 'lose-answer') throw new Error('network error')
    return { received: state.received }
  }
  return { state, send }
}

const noWait = () => Promise.resolve()

test('only a file over the batch budget goes in pieces', () => {
  assert.equal(goesInPieces({ size: BATCH_BYTES }), false)
  assert.equal(goesInPieces({ size: BATCH_BYTES + 1 }), true)
  assert.equal(goesInPieces({}), false)
})

test('a file is sent in order, the last piece short', async () => {
  const { state, send } = fakeServer()
  const seen = []
  await sendInPieces(fakeFile(25), 10, send, { wait: noWait, onProgress: (f) => seen.push(f) })
  assert.deepEqual(state.pieces, [[0, 10], [10, 20], [20, 25]])
  assert.equal(seen.at(-1), 1)
})

test('a piece that fails is sent again, alone', async () => {
  const { state, send } = fakeServer({ 1: 'drop', 2: 'drop' })
  await sendInPieces(fakeFile(30), 10, send, { wait: noWait })
  assert.deepEqual(state.pieces, [[0, 10], [10, 20], [20, 30]])
  assert.equal(state.calls, 5)
})

test('a piece whose answer was lost is not written twice', async () => {
  const { state, send } = fakeServer({ 0: 'lose-answer' })
  await sendInPieces(fakeFile(20), 10, send, { wait: noWait })
  assert.deepEqual(state.pieces, [[0, 10], [10, 20]])
})

test('a piece that keeps failing ends the upload', async () => {
  const send = async () => { throw new Error('network error') }
  let waits = 0
  await assert.rejects(
    sendInPieces(fakeFile(20), 10, send, { retries: 3, wait: async () => { waits++ } }),
    /network error/)
  assert.equal(waits, 3)
})

test('what cannot be helped by trying again is not tried again', async () => {
  let calls = 0
  const send = async () => { calls++; throw Object.assign(new Error('gone'), { fatal: true }) }
  await assert.rejects(sendInPieces(fakeFile(20), 10, send, { wait: noWait }), /gone/)
  assert.equal(calls, 1)
})
