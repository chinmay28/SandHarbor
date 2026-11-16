# Proton Drive on a phone that runs the vault

> Status: built, not yet tested on a device. See [the checklist](#first-run-on-a-phone).

The `protoncli` backend reaches Proton through Proton's own command-line
client (see [cloud-backends.md](cloud-backends.md#tier-4--driving-the-services-own-client--done-for-proton)).
That client runs on Bun, and Bun has no Android build, so a phone running the
whole vault had no way to Proton. This is the way: the Android app holds
Proton's own Drive SDK, and the vault reaches Proton through the app.

## Shape

```
 sand serve (Go, child process of the app)            SAND Vault app (Kotlin)
 ─────────────────────────────────────────            ─────────────────────────────────
 protonapp backend                                     app bridge  (127.0.0.1:<random port>)
   Put/Get/Stat/Delete/List/Ping/Usage  ── HTTP ──▶      bearer token check
   session from the vault, in a header                   ProtonStore
   rotated session ◀── response header ──                 ├─ sign-in by link (session fork)
                                                          ├─ session refresh
                                                          └─ me.proton.drive:sdk
                                                               (encryption, upload, download)
```

- **Kind** `protonapp`, labelled "Proton Drive (this phone)". `Local`, like
  every account reached through the machine it was connected on, so a second
  machine on the same vault never tries to reach it. Offered in the connect
  dialog only where an app bridge exists (`Spec.Offered`).
- **The bridge** is an HTTP server in the app, on loopback, on a port the
  system picks. The app starts it before the server, and hands the server
  `SAND_APP_BRIDGE` (its address) and `SAND_APP_BRIDGE_TOKEN` (32 random
  bytes) in the environment, which no other app can read. Loopback is
  reachable by every app on the phone, which is what the token is for.
- **The session lives in the vault**, as the `session` secret, encrypted
  under the vault password like every other cloud credential. It is sent with
  each request and kept by the app only in memory. A locked vault reaches
  nothing; a recovery kit carries the account.
- **Rotation.** Proton's access token expires and is refreshed with a refresh
  token that is single-use. When the app refreshes, it answers with the new
  session in `X-Sand-Proton-Session`, and the backend hands it to the vault
  through `CredentialRotator`, exactly as Box's refresh token travels.

The session is opaque to Go. The app writes and reads it as JSON:
`{"v":1,"uid","access_token","refresh_token","key_password","user_id","email"}`.
`key_password` is what Proton's web sign-in hands over: it unlocks the
account's keys. It is the same secret Proton's own CLI keeps in its session.

## Wire protocol

Every request carries `Authorization: Bearer <token>`. Account requests also
carry `X-Sand-Proton-Session` (the session, base64) and
`X-Sand-Proton-Folder` (where in Proton Drive the parts go). Keys are path
segments, escaped.

| Request | Answer |
|---|---|
| `GET /proton/v1/ping` | `204` |
| `GET /proton/v1/account` | `{"email"}` |
| `GET /proton/v1/usage` | `{"used","total"}`, from the account's own quota |
| `GET /proton/v1/objects?prefix=` | `[{"key","size"}]` |
| `GET /proton/v1/objects/{key}` | the bytes |
| `GET /proton/v1/objects/{key}?stat=1` | `{"key","size"}` |
| `PUT /proton/v1/objects/{key}` | `204` once Proton has the part |
| `DELETE /proton/v1/objects/{key}` | `204`, also when it was not there |
| `POST /proton/v1/signin` | `{"id","url"}` |
| `GET /proton/v1/signin/{id}` | `{"state":"pending"}`, `{"state":"done","session","email"}` or `{"state":"failed","error"}` |

Errors are `{"error","code"}` with `code` one of `not_found` (404),
`signed_out` (401), `bad_request` (400), `proton` (502: Proton said no) and
`internal` (500). `signed_out` is what a revoked session or a refresh token
that no longer works becomes, and it is the one a person has to act on.

## Signing in

Proton's sign-in for third-party clients is a *session fork*, the same one
its CLI uses ([incubating/account/js](https://github.com/ProtonDriveApps/sdk/tree/main/incubating/account/js)):

1. `GET /auth/v4/sessions/forks` → a user code and a selector.
2. The link `https://account.proton.me/desktop/login?app=drive&pv=3#payload=0:<code>:<key>:external-drive`,
   where `<key>` is 32 random bytes the app keeps.
3. The person opens it, signs in to Proton, approves.
4. `GET /auth/v4/sessions/forks/<selector>` answers 422 until then, and then a
   session and a payload: AES-256-GCM under `<key>`, nonce first, tag last,
   associated data `fork`, holding `{"keyPassword"}`.

The app never sees a password, a second factor or a CAPTCHA: Proton's own page
deals with all three.

## Proton's terms for third-party clients

The SDK is open to personal, non-commercial projects that follow
[Proton's rules](https://github.com/ProtonDriveApps/sdk#usage-guidelines-for-personal-projects).
What that means here:

- Every request says what it is: `x-pm-appversion: external-drive-sand_vault@<year>.<month>.<patch>-stable`.
- No Proton logo anywhere. The account is "Proton Drive" in words.
- The sign-in says it is a third-party application not officially supported by Proton.
- Requests go to Proton's own API host and nowhere else.

## Where the code is

| Part | File |
|---|---|
| The backend | `internal/provider/protonapp.go` |
| The sign-in endpoint | `internal/server/handlers_proton.go` |
| Bridge protocol, sign-in link and payload, session, app version | `apps/android/core/.../proton/` |
| The bridge server and the SDK | `apps/android/app/.../proton/` |

## First run on a phone

None of the Proton side can run where this was written: Proton's API is not
reachable from there and there is no emulator. The first real run is on a
phone:

1. Install the debug APK, run the vault on the phone, unlock it.
2. Connect a cloud → **Proton Drive (this phone)** → Sign in. A link appears;
   it opens in the browser. Sign in to Proton and approve.
3. Back in the app, the account is listed under the email Proton gave, and
   the folder `/my-files/sand` exists in Proton Drive.
4. Upload a file. Its parts appear in that folder in Proton's own app.
5. Open the file: it downloads back through Proton.
6. Delete it: its part leaves Proton (into Proton's trash, then gone).
7. Leave the phone for an hour or more, then open a file again. The access
   token has expired by then; it must refresh, and the account must still work
   after a restart of the app (the rotated session reached the vault).
8. **Server log** in the app's settings: no `signed_out`, no `proton` errors.

What to send back if a step fails: the step, the message the page showed, and
the server log.
