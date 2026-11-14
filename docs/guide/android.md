# SAND Vault on Android

The SAND Vault app puts your vault on an Android phone. It works one of two
ways, and you can switch between them at any time in the app's settings:

| Mode | What runs where | Use it when |
|---|---|---|
| **Connect to a server** (thin client) | The vault runs on another machine — a Raspberry Pi, a desktop, a server. The phone opens it, as a browser would. | You already run SAND Vault somewhere, and want it on your phone. |
| **Run the vault on this phone** (full stack) | The whole of SAND Vault runs on the phone: the index, the keys, the connections to your clouds, and the web interface. | You want a vault with no other machine, a second machine for a vault you already have, or a vault in your pocket for when home is out of reach. |

Either way the app shows the vault's own web interface, so everything the
browser app does, the phone app does: create a vault, unlock it, connect clouds,
upload, download, previews, films and VLC, folder tools, maintenance, sub
vaults, recovery kits, Sandy. This page covers what is different on a phone.
How it works inside is in [the app's design](../design/android.md).

## Install the app

Every [release](https://github.com/chinmay28/SandHarbor/releases) attaches it as
`sand-vault.apk`, and the latest is always at:

```
https://github.com/chinmay28/SandHarbor/releases/latest/download/sand-vault.apk
```

Open that address on the phone, download the APK, and open it. Android asks once
to let your browser install apps; allow it. A newer release installs over the
old one, keeping your settings and, in full-stack mode, your vault.

It needs Android 10 or newer. Full-stack mode needs a 64-bit ARM phone (nearly
every phone sold since 2017); on any other, the app is a thin client only and
says so.

## The first screen

The app asks where your vault is.

- **Connect to a server.** Type the server's address — `192.168.1.20`,
  `pi.local`, or `https://vault.example.com` — or tap **Find on this network**,
  which lists every SAND server that answers on your Wi-Fi (the same discovery
  SAND TV uses, so the server must not have been started with
  `--tv-discovery-port 0`). An address with no `http://` or `https://` means
  `http://` on SAND's own port, 8123.
- **Run the vault on this phone.** The vault starts on the phone, and you land
  on its lock screen, with three ways in:
  - **Create your vault** — a new, empty vault.
  - **I have a recovery kit** — restore a vault from its
    [recovery kit](recovery.md#importing-a-kit), every cloud reconnected.
  - **I have a recovery kit**, with *The vault is still running on another
    machine — open it here too* ticked — open a vault that already runs
    elsewhere on this phone as well, as a
    [second machine](vault-management.md#opening-the-vault-on-several-machines).
    Use the vault's own password. Changes travel between the machines through
    the clouds, and the phone appears under *Machines* on the other one after
    its first pull.

  Pick the kit's zip in the file picker: from Downloads, from a USB stick, or
  from a cloud's own app such as Drive.

## Switching modes and other settings

The app's settings are behind the **⚙** button in the vault's header (and
**⚙ App settings** on the lock screen), and also behind a long press on the
app's icon, which works whatever the server offers — an older server's page has
no ⚙ button.

| Setting | Mode | What it does |
|---|---|---|
| Mode | both | Connect to a server, or run the vault on this phone. Each mode keeps its own settings, so switching back finds them as you left them. |
| Server address, Find servers | thin client | Which server the app opens. |
| Serve other devices on this network | full stack | Off: only this phone can reach its vault. On: a laptop's browser, SAND TV or another phone on the same Wi-Fi can open it at `http://<phone's address>:8123`, with the vault password. The connection is not encrypted, as with any SAND server on plain HTTP. |
| Keep running when the app is closed | full stack | Off: the vault stops when you leave the app. On: uploads, scheduled checks and repairs carry on, and other devices can still reach it; a notification says it is running and has a **Stop** button. |
| Machine name | full stack | What your other machines list this phone as. The phone's own name unless you change it. |
| Port | full stack | 8123 unless another app on the phone has it. |
| Server log, Restart, Stop | full stack | What the vault on the phone has said lately, and control over it. |

Switching mode leaves the other mode's vault alone: switching a phone that runs
its own vault to a thin client stops that vault but does not delete it, and
switching back opens it again.

## Files on a phone

- **Uploading.** The vault's upload button opens Android's file picker, which
  reaches the phone's storage and any cloud app that offers files (Drive,
  Dropbox, a USB stick). Pick several at once. Picking a whole folder is not
  something Android's picker offers; upload its files instead.
- **Downloading.** A file's Download button opens the share sheet, as it does in
  a phone's browser, because on a phone a file is usually on its way to another
  app. **Save to Downloads** is first in the sheet. Files saved there, and
  folders downloaded as a zip, land in *Downloads/SAND Vault*.
- **Films.** A film plays in the app, full screen when you ask. **Open in VLC**
  hands it to the VLC app, as from Chrome.
- **Signing in to a cloud** (Google Drive, OneDrive, Dropbox, …) opens the
  phone's browser, because Google and others refuse to sign in inside an app.
  Sign in, then come back to the app: it notices the new account by itself. In
  full-stack mode the provider sends the browser back to
  `http://127.0.0.1:8123/api/providers/oauth/callback`, which is the phone's own
  vault; if you use your own OAuth app, allow that address as a redirect, as for
  any machine reached at 127.0.0.1.

## Full stack: what to know

- **Where the vault lives.** In the app's private storage, which no other app
  can read and Android's backups leave out. **Uninstalling the app deletes the
  vault file.** Your files are on your clouds and survive that, and every cloud
  carries an encrypted copy of the index, so your password recovers the vault
  ([recovery](recovery.md#recovering-from-your-accounts)) — but export a
  [recovery kit](recovery.md#recovery-kit) first, and keep it off the phone.
- **Local-folder accounts** are folders inside the app's own storage, so they
  vanish with the app. Use real clouds on a phone.
- **When the phone sleeps**, Android slows down background work, even with *Keep
  running* on. Big uploads go fastest with the screen on or the phone charging.
- **Mobile data.** The vault talks to your clouds over whatever network the
  phone has. Uploading and repairing a large folder can use a lot of it.
- **A second machine.** Local folders on the other machine show as *on
  study-pc* on the phone, and files whose parts are only there are greyed out,
  as for any [second machine](vault-management.md#opening-the-vault-on-several-machines).
  After a password change on one machine, the other rejoins from a fresh kit.

## Thin client: what to know

- **HTTPS with a private certificate.** If your server's certificate is from
  your own CA, install that CA in Android (*Settings › Security › Encryption &
  credentials › Install a certificate › CA certificate*); the app trusts it. A
  certificate nothing on the phone trusts is refused, not clicked through.
- **Plain HTTP** is allowed, as for SAND on a home network, with the same
  caveat as in a browser: the vault password crosses the network in the clear.
  See [security](../security.md).
- **Can't reach the server?** The app says so, with **Retry** and **App
  settings**. Check the phone is on the same network, or on your VPN.

## Troubleshooting

| Symptom | What to do |
|---|---|
| *The vault on this phone would not start*, with a log | Another app may hold the port: change **Port** in settings. Otherwise share the log (**Server log › Share**) with a bug report. |
| Every cloud shows as unreachable on the phone's vault | The phone has no network, or a VPN reports no DNS servers; the vault then asks 1.1.1.1 and 8.8.8.8. Check the server log for DNS errors. |
| The ⚙ button is missing | The server is older than the app. Long-press the app's icon and pick **Settings**. |
| A sign-in page says the browser is not supported | It opened inside the app instead of the browser. Update the app. |
