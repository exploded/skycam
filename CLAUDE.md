# skycam — all-sky camera, replacing allsky on the observatory Pi

See README.md for what it does and how it deploys. The plan this was built
from: phases M0–M6 (M5 native USB driver and M6 meteors are not built).

## Layout

- `cmd/skycam` — the Pi binary (`run`, `process -night`)
- `cmd/skycam-web` — the relay on the Linode (skycam.mchugh.au, port 8996)
- `internal/camera` — `Camera` interface; `asisdk` (purego over
  libASICamera2.so, linux arm64/amd64 only), `fake` (replays JPEGs)
- `internal/capture` — the loop: day/night, auto-exposure, analysis, overlay,
  save, live mode; `Hub` fans frames out to the web UI and uploader
- `internal/exposure`, `internal/astro`, `internal/analysis` — pure, table-tested
- `internal/observatory` — polls rolloff/rg11 `status.json` (port 80) and
  N.I.N.A.'s Advanced API
- `internal/night` — dawn products (keogram, startrails, ffmpeg timelapse),
  retention, catch-up
- `internal/report` — self-contained HTML report; `svg.go` and `report.css`
  are copied from skyq and must stay in step with skyq's `design/CHARTS.md`
- `internal/web` — the Pi UI; `internal/relay` — the relay
- `internal/store` — SQLite (modernc + sqlc); `schema.sql` is the source of
  truth; run `sqlc generate` after editing it or `queries.sql`
- `third_party/zwo` — ZWO SDK 1.41 arm64 `.so` (MIT), udev rule

## Contracts that other things depend on

- **skyq reads `/images/<YYYYMMDD>/image-<YYYYMMDDHHMMSS>.jpg` with basic
  auth**, exactly as it read allsky. The night date is allsky's (frames before
  noon belong to the previous evening). `web_test.go` pins this.
- **The overlay's top-left block keeps allsky's layout.** skyq measures cloud
  in a band from 6% to 30% of the frame height that includes it. New text goes
  bottom-left only.
- **Unknown is never closed or safe.** A stale roof or rain reading is
  `unknown`; cloud is only estimated with the roof known `OPEN` and the sun
  below −12°. Everything here is advisory — never a SafetyMonitor (skyq §8).
- **The ESP32 devices are read on port 80 `/status.json`, never Alpaca port
  11111**, which N.I.N.A. owns.

## Gotchas

- The camera is inside the observatory: with the roof shut it sees the roof,
  and with it open the telescope and roof edge fill much of the frame.
  `FindStars` skips background blocks well below the sky level for that reason.
- allsky's night frames are very dark (sky about 1-40 ADU at 5 s, gain 50).
  Star detection works near the noise floor; the hot-pixel map matters.
- allsky takes ASI120 stills as one-frame video captures because snap mode is
  unreliable on USB 2 cameras; `asisdk` does the same unless `camera.snap`.
- MJPEG handlers must flush headers before the first frame, or a client with
  no frame yet waits forever.
- `Loop.NightEnded` runs on the capture goroutine and must return at once.

## Workflow

- `go test ./...`; `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/skycam`
  must also pass (CI checks it).
- Test locally with the fake driver on a spare port (8999), never 80.
- Never deploy to the Pi or restart its service unless James asks.
- Repo `exploded/skycam`, branch `main`.
