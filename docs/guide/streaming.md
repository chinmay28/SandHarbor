# Streaming and mounting

SAND Vault can hand a single film to a player, put a folder of films on a TV, or
serve the whole vault as a drive. This page covers each one and what it exposes.
For the mechanism (stream tickets, the WebDAV share, standalone mode), see
[Server design](../design/server.md). For why a 4K rip can stutter where a 1080p
film plays, see [Streaming performance](../design/streaming-performance.md).

| You want to | Use | Credential |
|---|---|---|
| Play one file in VLC or any player | [Stream a file to VLC](#stream-a-file-to-vlc) | A link for that one file |
| Browse a folder of films on a TV | [Watch on a TV](#watch-on-a-tv) | A four-character code for that folder |
| Browse the whole vault from a file manager | [Mount it as a drive](#mount-it-as-a-drive) | Your vault password, on every request |

Whichever you use, a film or folder of large photos you open often can be
[kept on this machine](#keep-files-on-this-machine), so it opens from local disk
rather than the clouds.

## Stream a file to VLC

`▶` on a video or audio row, or **Stream in VLC** in the row's menu, opens the
file in VLC and starts it playing. **Copy the address** in the same menu gives
you the link for any other player.

The browser cannot hand VLC its session cookie, so the address carries the
authority itself. The server mints a link that stands for **one file**, and the
player follows it with no password and no session. Anyone holding the link can
play that file, which is why it is limited:

| | |
|---|---|
| Lifetime | 12 hours, restarting each time the link is used, so a link in use never expires mid-film |
| Locking the vault | Voids every outstanding link immediately |
| Reach | One file. Not the folder, not the index, not the vault. |

The dialog always shows the address with a copy button, which is also the
fallback when no VLC is installed. How the handoff is made depends on the device:

| Device | Handoff |
|---|---|
| iOS / iPadOS | `vlc-x-callback://`, VLC's documented entry point |
| Android | An `intent:` naming `org.videolan.vlc`, so https stays https |
| Desktop | A two-line `.m3u` playlist, which VLC registers for on every desktop |

**Seeking** fetches only the chunks covering the part of the film asked for, not
the whole file. **Auto-lock** does not fire while a film is being sent, and a
player left paused stops counting as use, so a paused TV does not hold the vault
open all night. A stream is abandoned only when the vault has sent nothing for
half an hour.

Any player that takes a URL works: it is an ordinary HTTP address that answers
`Range` requests.

> [!NOTE]
> The link uses the origin you reached the app on, so behind Tailscale Serve or a
> reverse proxy it is an `https://` address automatically. Over plain HTTP it is a
> bearer token in the clear, like everything else sent there, so put TLS in front
> (see [Deployment](deployment.md)).

### Blu-ray disc images

A Blu-ray kept as an `.iso` is a disc, not a film: a filesystem with clips,
playlists and menus in it, which most players will not open from an address.
So the link for one plays the **film inside it**. The server reads the disc's
playlists, picks the main title (the longest), and serves that title's clips as
an ordinary transport stream. The address ends in `.ts`, and VLC on a desktop or
a phone, SAND TV, or anything else that takes a URL plays it and can seek in it.

Nothing is converted or unpacked. The film is read out of the stored image at
the offsets it already lies at, so it costs no extra space and starts as fast
as any other film.

| | |
|---|---|
| What you get | The main title: picture, every audio track and the disc's subtitle tracks, as they are on the disc |
| What you do not | Menus, extras, and chapter marks |
| Encrypted discs | Not playable. An image copied from a bought disc without decrypting it is still encrypted, and the link is then to the image itself, as below. |
| Other `.iso` files | A DVD image, a data disc or an installer has no Blu-ray film in it. `▶` gives a link to the file itself, which is only useful to a player that opens disc images. |
| Downloading | **Download** always gives you the whole `.iso`, never the film cut out of it. |
| In the browser | Clicking a disc image matched to a film opens the film's page, with **Stream in VLC** and **Close**. It has no **Play** of its own: a browser cannot play a disc. |

One limit comes from serving the disc's clips as they are: a title that plays
only part of a clip gets the whole clip. A title made of many clips (some
discs split a film into dozens) is sent as one film: each clip keeps its own
clock on the disc, and the server moves them onto one, so a player shows the
right running time and seeks across the whole of it. A frame may be dropped
where one clip meets the next.

## Watch on a TV

**SAND TV** puts one folder on a television, with the posters, synopses and cast
that [film lookup](films.md) stored, and plays the films straight off the machine
running SAND Vault, with seeking and resume. It runs as a Fire TV app, or in any
TV browser at `http://<host>:8123/tv/`.

The TV is never given the vault. A folder's menu → **Watch on TV** makes a
**four-character code** for that folder, for an hour up to thirty days, a custom
length up to a year, or with no end. The TV finds the server on the network, you
key in the code with the remote, and it can watch the videos in that folder and
below, and nothing else. Revoking the code in the same dialog unpairs the TV and
stops what it is playing. Codes live in memory only, so a restart ends them all.

Installing the app, pairing, the player and what plays are in the
[SAND TV guide](sand-tv.md).

## Mount it as a drive

`sand serve --webdav` also serves the vault as a WebDAV share, so a file manager
or player can open it as a drive. It is **off by default**; see the warning below.

```bash
sand serve --webdav                        # share at http://<host>:8123/dav/
sand serve --webdav --webdav-path /share   # somewhere else
```

On the systemd service from the quickstart, set `SAND_WEBDAV=1`, either at install
or by re-running the quickstart, which keeps every other setting:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/SandHarbor/main/scripts/quickstart.sh \
  | sudo SAND_WEBDAV=1 bash
```

Once it is on, **Vault settings → Mount as a drive** shows the address with a copy
button and where to paste it. The address is the one you reached the app on, so
behind a TLS proxy it is the `https://` one.

Mount it with **any username** and your **vault password**:

| Client | How |
|---|---|
| macOS | Finder → Go → Connect to Server → `https://host:8123/dav/` |
| Linux | `gio mount dav://host:8123/dav/`, or Files → Other Locations |
| Windows | Map network drive → `https://host:8123/dav/` |
| VLC | Open Network Stream → `https://host:8123/dav/film.mkv` |
| iOS / tvOS | VLC or Infuse → Add Network Share → WebDAV |

- **Seeking** fetches only the chunks a range covers.
- **Copying** in and out streams rather than buffering, so size is bounded by
  disk, not memory.
- **Renaming and moving** change only the index; the stored parts never move. A
  folder rename carries everything beneath it, thumbnails included, in one write.
- **A correct password against a locked vault unlocks it**, so a mount stays
  alive past the idle timeout. Requests to the share count as activity.

> [!WARNING]
> WebDAV uses HTTP Basic auth, which sends your vault password on **every
> request**, not once at sign-in. Over plain HTTP that is far more exposure than
> the web UI, which is why the share is opt-in. Put TLS in front of it (Tailscale
> Serve, or `scripts/nginx-sand.conf`; see [Deployment](deployment.md)) and use
> `https://`. Windows refuses Basic auth over plain HTTP by default anyway, and
> caps files at 50 MB until `FileSizeLimitInBytes` is raised.

To play *one* file you do not need the share at all: a
[stream link](#stream-a-file-to-vlc) works on a plain `sand serve` and hands over
a link rather than your password. The share is for browsing the whole vault from
outside the browser.

Jellyfin and Plex want a real filesystem rather than a URL, so they would need a
FUSE mount, which is not built.

### Mount a sub vault on its own

The share at `/dav/` is always the main vault and never shows a sub vault, locked
or open. Each sub vault has its own share beside it:

```
https://host:8123/dav/          the vault      opens with your vault password
https://host:8123/sub/Taxes/    a sub vault    opens with that sub vault's password
```

Mount it with **any username** and **that sub vault's password**. Your vault
password does not open it, and its password does not open `/dav/`. The drive holds
that sub vault and nothing else, and a move whose destination is another share is
refused. So a sub vault mounted on one machine, or handed to one person, keeps the
rest of the vault out of reach. The app, which has your vault password, still lists
every sub vault as before.

- **Address:** the sub vault's name (case ignored) or its ID; `sand sub ls` shows
  both. A name containing a slash is reachable only by ID. **Vault settings → Sub
  vaults** offers each address with a copy button. Renaming a sub vault moves its
  share.
- **Locked sub vault:** mounting it opens it, since the password the drive sends
  is the one that opens it.
- **Locked vault:** a sub vault's files live on accounts your vault password
  seals, so its share answers `503 Service Unavailable` until the vault is
  unlocked in the app (or by mounting `/dav/`).
- **Path:** `--webdav-sub-path` moves the shares from `/sub`. They sit beside the
  vault's share rather than under it, because a sub vault called `Films` under
  `/dav/` would shadow a folder called `Films`. The server refuses to start with
  one path inside the other.

> [!NOTE]
> A mounted drive is a folder every process running as that user can read, that a
> backup agent will copy elsewhere, and that stays mounted after you walk away.
> That is why sub vaults are kept off the main share. Mounting a sub vault chooses
> that exposure for that one sub vault.

Sub vaults themselves are covered in [Vault management](vault-management.md).

## Keep files on this machine

A film streams as fast as the slower of the two clouds it needs, and a folder of
RAW photos is a round trip to the clouds per picture. **Keep on this machine**,
in a file's or folder's menu, puts enough of its parts on the disk of the
machine running SAND that it opens, streams and seeks from there instead, until
a date you choose: a day, a week, a month, three months, a year, or a date of
your own.

| | |
|---|---|
| What is kept | The file's parts exactly as the clouds hold them: compressed, erasure-coded and encrypted. Only the fewest that rebuild each chunk, k of n and never more (two of three on the default 2-of-3), so a film takes about its own size |
| Parts or replica | Where a file or folder is [replicated](storing-files.md#replication), the dialog offers the same **Read from** choice a stream link does. **Parts** (the default) keeps the parts. **Replica** keeps the replica alone, often the copy on a cloud of its own, and none of the parts. **Either** keeps the parts, and the replica only for a chunk the parts cannot give (too many of their clouds down or emptied). Whichever it is, only the fewest shards that rebuild each chunk are kept, and never the parts and the replica both. A folder kept from its replica leaves out the files under it that have none |
| Overlapping | A file's own choice beats its folder's, and a deeper folder's beats a shallower one's |
| When it is put together | Only when it is opened, in memory, exactly as from parts off the network. Nothing readable is written to the disk |
| Where | A folder beside the vault, `vault.sand.cache` (`/var/lib/sand` under the service, `~/.sand` on a desktop). On the machine running SAND, which for a browser on a laptop looking at a home server is the server |
| A folder | Everything under it, including what is added later |
| A file | Followed by its ID, so renaming or moving it keeps it kept |
| Until | The date you picked. Then its parts are let go of. Keeping it again moves the date |
| Locking the vault | Keeps what is kept: it is ciphertext. Nothing is fetched while the vault is locked |
| Other machines | Pins are this machine's own. Another machine opening the same vault keeps its own, or none |
| Sub vaults | Not kept: the list of what is kept is sealed under the main vault's key, which must not name a sub vault's folders |

**Filling.** The server fetches what is kept in the background while the vault is
open, and holds off the idle lock while it is fetching, the way it does for
replication. Opening a kept file before then keeps whatever that read had to
fetch, so watching a kept film fills it as it plays, and that includes a read
the replica stood in for. A file kept from its replica opens from this machine
when it is played from the replica; when the replica is cut at the same chunk
size as the parts, which it is unless the chunk size changed in between, any read
of it does. Each part read from the
disk is checked the way one from a cloud is: one that has rotted is dropped and
fetched again.

**Room.** The cache never takes the disk below a reserve, a tenth of the disk
(between 1 and 20 GB) unless you set another, and never past a limit if you set
one. **Vault settings → Kept on this machine** lists what is kept, how much of
each has arrived, and the room left, and sets both. What does not fit stays on
the clouds and still opens from there; nothing already kept is pushed out to
make room.

From the command line:

```bash
sand cache keep /films/Dune.mkv --for 7d
sand cache keep /photos/2026 --until 2026-12-31
sand cache keep /films --for 30d --from replica
sand cache ls
sand cache limit --max 200G --reserve 20G
sand cache rm /photos/2026
sand cache fill          # fetch now, in the foreground
```

A file still in the format SAND used before chunking has to be
converted (`sand vault convert`; see the
[CLI reference](../reference/cli.md#sand-vault-convert)) before it can be kept,
as before it can be streamed.

