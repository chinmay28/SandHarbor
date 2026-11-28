import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  archiveFolder, archivePicks, describeExtract, extractFraction, extractNotes, extractOutcomeText,
  extractProgressText, isArchive,
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
