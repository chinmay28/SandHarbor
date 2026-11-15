# The SAND Harbor icon

[`icon.png`](icon.png) is the official icon of every SAND Harbor app: a
sandcastle of bricks on a beach, by the harbour, in the amber and navy of the
apps' theme. SAND Vault, SAND TV, and every app after them wear the same one.

It is the only icon anyone draws. Every size and shape a platform wants is
derived from it by `make icons` ([`cmd/sand-icons`](../cmd/sand-icons), on
[`internal/brand`](../internal/brand)), and `go test ./cmd/sand-icons` fails if
a committed copy has drifted from it. Never edit a derived file by hand: the
next `make icons` puts it back.

## What gets made from it

| Where | What | Shape |
|-------|------|-------|
| `web/public/`, `internal/server/dist/` | `icon-192.png` (also the favicon and the header mark), `icon-512.png` | rounded, on transparency |
| | `apple-touch-icon.png` | full bleed: iOS cuts its own corners |
| | `icon-maskable-512.png` | scene pulled into the launcher's safe circle |
| `internal/server/tvui/`, Fire TV `assets/` | `icon.png`, 256px | rounded |
| both Android apps' `res/` | `mipmap-*/ic_launcher.png` | rounded, 48dp in every density |
| | `mipmap-*/ic_launcher_foreground.png`, `mipmap-anydpi-v26/ic_launcher.xml`, `values/ic_launcher_background.xml` | adaptive icon: the scene in the 72dp a launcher shows, over the plate colour |
| Fire TV `res/drawable-xhdpi/banner.png` | the 320×180dp home-screen tile | the icon centred on the plate colour |
| SAND Vault for Android `res/drawable-nodpi/sand_logo.png` | the mark on the app's own screens (main, setup) | rounded |

One Android icon is not generated: the notification's status-bar icon,
`apps/android/app/src/main/res/drawable/ic_stat_sand.xml`. Android draws a
small icon as a single-colour mask, so the artwork would show as a blank
square; that file is the sandcastle redrawn by hand as a silhouette (flag,
studded bricks, arch, sand) on the 24dp grid. If the artwork changes shape,
redraw it to match.

`go test ./cmd/sand-icons` also checks that every `@drawable/`, `@mipmap/`,
`R.drawable.` and `R.mipmap.` reference in the Android apps names a file that
exists, so retiring an icon can't leave a reference that only a Gradle build
would notice.

## What the generator assumes about the artwork

- It is a square PNG.
- The scene sits on a flat navy **plate** that runs edge to edge with rounded
  corners, and whatever is outside those corners (black, here) is thrown away.
  Rounded icons get transparency there; full-bleed ones get the plate colour,
  so a platform's own mask never shows a black corner.
- The plate's colour and corner radius are written down in
  [`cmd/sand-icons/apps.go`](../cmd/sand-icons/apps.go) (`plateColour`,
  `plateRadius`). They were measured off this artwork: the colour is the
  median of the plate's pixels, and the radius is where the plate begins on
  the diagonal from a corner, divided by 0.293 (1 − 1/√2), as a fraction of
  the width.

## Replacing the artwork

1. Replace `brand/icon.png`, keeping to the assumptions above.
2. If the plate's colour or corner radius changed, measure them again and
   update `apps.go`.
3. `make icons`, look at the results (corners especially), and commit the
   artwork with everything it redrew.

## Adding an app

Add an entry to `apps` in `cmd/sand-icons/apps.go` naming where its icons go.
The helpers there already cover a web app (`webIcons`) and an Android app
(`launcherIcons` and `adaptiveIcon`, then `android:icon="@mipmap/ic_launcher"`
in its manifest). Then `make icons`.
