# SAND Harbor marks

Every SAND Harbor app is built from the same bricks. Each app has one mark,
drawn once as an SVG in this directory; `make icons` derives every other form
of it (see [`cmd/sand-icons`](../cmd/sand-icons)), and `go test
./cmd/sand-icons` fails if a committed copy has drifted.

| Mark | App | What the bricks say |
|------|-----|---------------------|
| [`vault.svg`](vault.svg) | SAND Vault | Three bricks about to click together: a file is stored as three encrypted parts on three accounts. The bottom one is dimmed because any two rebuild the file. |
| [`tv.svg`](tv.svg) | SAND TV | One tall brick for the screen with play cut out of it, on a dimmed brick for a stand. |

## The family rules

A new app's mark follows these, so it sits next to the others on a home
screen and reads as part of the same set.

**The canvas.** A `0 0 64 64` viewBox. It is drawn at 64 but has to survive
24px in a page header, so keep to thick shapes and wide gaps: nothing under
about 2.5 units.

**The plate and the ring.** Every mark starts with the same two shapes,
copied exactly:

```svg
<rect width="64" height="64" rx="14" fill="#0a0e17"/>
<rect x="8.5" y="8.5" width="47" height="47" rx="9" fill="none" stroke="#1e2d3d" stroke-width="3"/>
```

The first shape covering the whole canvas is the plate; the generator squares
it off for iOS and keeps everything else inside the ring for Android's
maskable icon, so put all the artwork inside the ring (10..54).

**Bricks.** Side-on, 32 units wide (x 16..48) — four studs on an 8-unit pitch.

- A brick body is a rect with `rx="2"` (2.5 for a big one), at least 8 tall.
- Studs are `5 × 4` with `rx="1"`, at x = 17.5, 25.5, 33.5, 41.5, sitting
  1 unit into the body below them so the join never shows a seam.
- A stand or base can be narrower (20 wide, centred), and needs no studs —
  the brick above hides them.

**Colour.** Only the theme's amber, from `web/src/theme.js`, and studs always
one step brighter than the brick they sit on:

| Part | Colour | Theme name |
|------|--------|------------|
| Plate | `#0a0e17` | `bg` |
| Ring | `#1e2d3d` | `border` |
| Studs on a full brick | `#f59e0b` | `accentBright` |
| Full brick | `#d97706` | `accent` |
| Studs on a dimmed brick | `#d97706` | `accent` |
| Dimmed brick (the spare, the stand) | `#92400e` | `accentDim` |
| A glyph cut into a brick | `#0a0e17` | `bg` |

Use solid colours rather than `opacity` for the dimmed brick: overlapping
translucent shapes darken where they meet.

**The glyph.** What makes one app's mark different from another's is the
arrangement of bricks, and at most one simple glyph cut into a brick in the
plate colour — play for TV. Keep it a polygon of a few points.

**Allowed SVG.** `<rect>` (with `rx`), `<polygon>`, and `<g fill="…">` for
grouping. Colours are `#rrggbb`. Anything else is refused by the generator,
which is deliberate: each of those shapes has an exact distance function, so
the PNGs are antialiased exactly and the Android vector drawables are
lossless.

## Adding an app

1. Draw `brand/<app>.svg` to the rules above. Start from `tv.svg`.
2. Add an entry to `apps` in [`cmd/sand-icons/apps.go`](../cmd/sand-icons/apps.go)
   naming the files it should write: SVG copies for web pages, a directory for
   home-screen PNGs, Android launcher icons, banners.
3. `make icons`, look at the result, commit the mark and everything it drew.

Never edit a generated file by hand — the next `make icons` puts it back.
