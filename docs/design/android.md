# SAND Vault for Android

The Android app (`apps/android`) is the vault's own web interface in a web
view, in front of one of two servers: a server on another machine (thin
client), or the real `sand` server running on the phone (full stack). This page
is how it is put together and why. Using it is in the
[guide](../guide/android.md).

```
 ┌──────────────────────── the phone ─────────────────────────┐
 │  MainActivity                                               │
 │   WebView ── the vault's web UI, from Origin only           │
 │     ▲  sand-bridge.js (document start, Origin only)          │
 │     │  SandAndroid ⇄ PageBridge: save, share, settings       │
 │     │                                                       │
 │     │ thin client            full stack                     │
 │     │     │                http://127.0.0.1:8123            │
 │     │     │                     ▲                           │
 │     │     │   ServerService ── ServerSupervisor ── libsand.so serve
 │     │     │   (foreground)       restart, log    (the sand binary)
 │     │     │                     ▲  SAND_DNS_FILE ← DnsWatcher
 └─────┼─────┼─────────────────────┼───────────────────────────┘
       │     ▼                     ▼
       │  http(s)://server       clouds
```

## One interface, two servers

The browser app already does everything: creating a vault, the lock screen's
recovery-kit import (as a new vault or a second machine), connecting clouds,
uploads, previews, streaming, every folder tool. A native client would have to
be written again for each of those, and would fall behind the moment the web
client gained something. So the app does not reimplement the vault: it shows
the server's page, and adds only what a web view lacks.

Thin client and full stack differ in one thing, the origin the web view opens:
the configured server, or `http://127.0.0.1:<port>`. Everything else — the
bridge, downloads, the share sheet, navigation rules — is the same code keyed on
that origin. Each origin has its own cookies and storage in the web view, so
switching mode never mixes the two vaults' sessions.

## The server on the phone

**The binary.** Full stack runs the real `sand`, cross-compiled by the app's
Gradle build (`buildSandServer-<abi>`, before `preBuild`) and packaged as
`lib/arm64-v8a/libsand.so`. Android lets an app execute files only from its
native library directory, and only if they are installed as files rather than
mapped from the APK, hence the `lib*.so` name and
`packaging.jniLibs.useLegacyPackaging`. `-PsandServer=false` builds a thin
client without Go.

`GOOS=android GOARCH=arm64` with no cgo, which Go supports: the runtime then
stays inside Android's seccomp filter and reads the system's time zone
database. The x86_64 emulator has no cgo-free `android` port, so
`-PsandServerAbis=arm64-v8a,x86_64` adds a static `linux/amd64` build with the
zone database built in (`-tags timetzdata`). 32-bit ARM is left out: its
`android` port needs cgo, and full-stack mode is not worth the NDK for the few
32-bit phones left.

**What Android lacks.** Go assumes two files a Linux distribution provides:

- `/etc/resolv.conf`. Without it Go's resolver asks `127.0.0.1:53`, and every
  cloud is unreachable. The app's `DnsWatcher` writes the current network's DNS
  servers to a file on every network change and names it in `SAND_DNS_FILE`;
  [`internal/hostenv`](../../internal/hostenv) installs a resolver that re-reads
  it when it changes and sends each query to those servers in turn. `SAND_DNS`
  (1.1.1.1 and 8.8.8.8) is asked only while the phone names none.
- The local zone. Go on Android always starts in UTC; `SAND_TZ` carries the
  phone's zone, so *daily at 03:00* means 03:00 where the phone is.

Both are environment variables read at startup by `cmd/sand` and do nothing
when unset, so the binary is the same `sand` everywhere else.

The rest of the environment points Go at the app's own directories (`HOME`,
`TMPDIR`, `XDG_*`, which would otherwise be `/sdcard` and `/data/local/tmp`)
and sets `SAND_INSTANCE` to the phone's name: every Android app sees the
hostname `localhost`, and two machines of one name are refused.
`core/ServerLaunch.kt` is the whole command line and environment, tested on
its own.

**Keeping it running.** `ServerService` is a foreground service of type
`specialUse` that owns one `ServerSupervisor`. The supervisor starts the
process, waits for `/api/health`, restarts it with backoff if it exits (and
leaves it down after five exits in two minutes, with the log's tail), and
stops it with SIGTERM — which `sand` answers by locking the vault and finishing
its index write — before SIGKILL. A server an earlier run left behind (the app
killed, its child not) is found in `/proc` by its program path and ended before
a new one starts. The server stops with the app unless *Keep running* is on,
and stops when the task is swiped away under the same rule.

**Binding.** Loopback by default, with discovery off: only this phone's apps
can reach it. *Serve other devices* binds `0.0.0.0` and answers SAND TV's
discovery broadcast, which is what makes a phone a server for a TV.

## The page bridge

A web view has no Downloads and no share sheet, and nothing outside the page
can read a `blob:` address — which is how the page hands over every rebuilt
file. `assets/sand-bridge.js` is added to the page at document start (before
the page's own scripts) and turns the page's two ways of handing over a file
into messages on an object the web view injects, `SandAndroid`:

| Page does | Bridge sends | App does |
|---|---|---|
| clicks `<a download href="blob:…">` (`saveBlob`) | `begin`, `chunk`…, `end` with action `save` | writes it to *Downloads/SAND Vault* through MediaStore |
| clicks `<a download href="/…">` on the origin (folder zip) | `fetch` | downloads it with the web view's cookies |
| `navigator.share({ files })` (which a web view lacks) | each file as `stage`, then `share` | opens the share sheet, *Save to Downloads* first |
| ⚙ App settings | `settings` | opens the settings screen |

It also defines `window.sandApp`, which is how the page knows to show the ⚙
buttons (`web/src/nativeApp.js`).

A file crosses in base64 chunks of 768 KB, each answered before the next is
read, so neither side holds more than a chunk in flight. `core/Transfers.kt`
holds the page to what it announced — chunks in order, never more bytes than
the size it gave, exactly that many by the end — and discards a transfer that
breaks any of those, so a half file never lands in Downloads.

**Trust.** The script and the `SandAndroid` object are both given to the
configured origin alone (`addDocumentStartJavaScript` and
`addWebMessageListener` with that origin as the only rule), and the web view
itself checks every page and message against it. A `fetch` is refused unless
it is on the origin, and its redirects are followed only within it, so the
session cookie never goes anywhere else.

## Navigation

`core/NavigationPolicy.kt` decides every navigation and every `window.open`:

- the origin's own pages, and `blob:`/`data:` previews, stay in the app;
- other `http(s)` addresses go to the phone's browser — above all a cloud's
  sign-in, which Google refuses inside a web view. The provider sends the
  browser back to the vault's callback, which the browser on the phone reaches
  as well as the web view (127.0.0.1 is the phone either way), and the page
  notices the new account by polling, not through the window;
- `intent:` addresses — how the page hands a film to VLC — are launched after
  dropping any component or selector the page named and limiting them to apps
  that take links from a browser, as Chrome does;
- `javascript:`, `file:`, `content:` and the like are refused.

## Code layout

```
apps/android/
├── core/        plain Kotlin, JVM-tested, no Android:
│                Origin, NavigationPolicy, AppConfig, ServerLaunch (+ DnsList,
│                MachineName), ServerSupervisor (+ RestartPolicy, StaleServers),
│                Health, Bridge, Transfers, FileNames, DiscoveryProtocol
└── app/         the Android shell around it:
                 MainActivity (web view), PageBridge + assets/sand-bridge.js,
                 ServerService + LocalServer + DnsWatcher, Downloads, Sharing,
                 SaveToDownloadsActivity, SetupActivity, SettingsActivity,
                 LanDiscovery, ConfigStore
```

Anything the app decides lives in `:core` and is tested there
(`./gradlew -PcoreOnly :core:test`, no Android SDK needed); `:app` is the
platform glue. The bridge's page side is tested against the real web client in
[`tests/test_android_app.py`](../../tests/test_android_app.py), with the native
side played in JavaScript.

## Not built

- Folder uploads: Android's file picker has no folder mode the web view passes
  through.
- 32-bit ARM full stack (see above), and iOS, which forbids running a server
  binary at all.
- A native file browser. The web interface is the interface.
