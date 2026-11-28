import { test } from 'node:test'
import assert from 'node:assert/strict'
import { archiveFolder, archivePicks, describeExtract, isArchive } from './upload.js'

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
