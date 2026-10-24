# skycam

skycam is the observatory's all-sky camera software. It replaces
[allsky](https://github.com/AllskyTeam/allsky) on the Raspberry Pi 4 at
allsky.local, which drives a ZWO ASI120MM Mini mounted inside the roll-off
observatory.

It does the following:

- Captures stills with its own auto-exposure, switching between day and night
  settings on the sun's altitude.
- Saves night frames in allsky's layout, so skyq reads them unchanged.
- Draws allsky's date, exposure, gain and temperature block, plus the roof,
  rain and N.I.N.A. state.
- Counts stars in each frame and estimates cloud while the roof is open.
- Builds a timelapse, keogram, startrails and night report at dawn.
- Uploads images and videos to S3, and publishes the report to
  deepspaceplace.com.
- Serves a live view of the observatory on the LAN, and from anywhere through
  the relay at skycam.mchugh.au.

The repo builds two programs:

| Program | Runs on | Does |
|---|---|---|
| `skycam` | The Pi, port 80 | Capture, analysis, products, local web UI, uploads |
| `skycam-web` | The Linode, port 8996 | Relay: latest frame, live stream, night list, report publishing |

Neither uses cgo. `skycam` loads ZWO's `libASICamera2.so` at run time with
[purego](https://github.com/ebitengine/purego), so both programs cross-compile
from Windows.

## Run it on your PC

The `fake` camera driver replays a folder of allsky stills, so you can run the
whole pipeline without the camera.

1. Copy a night of stills into a folder, for example from
   http://allsky.local/images/20261023/.
2. Write a config that points at it:

   ```json
   {
     "listen": "127.0.0.1:8999",
     "data_dir": "C:/temp/skycam-data",
     "camera": { "driver": "fake", "fake_dir": "C:/temp/frames/20261023" },
     "observatory": { "roof_url": "", "rain_url": "", "nina_url": "" }
   }
   ```

3. Run `go run ./cmd/skycam -config config.json run`.
4. Open http://127.0.0.1:8999/.

The replay runs as fast as the frames decode, then processes the night and
keeps serving. With the roof URL empty, the roof reads `unknown` and cloud
stays `unknown`. That's deliberate: skycam only judges cloud with the roof
known to be open.

To rebuild one night's products, run
`skycam -config config.json process -night 20261023`.

## Configuration

`skycam` reads `/etc/skycam/config.json` on the Pi. Every key has a default,
so the file only needs what differs, and an unknown key is an error.
[config.example.json](config.example.json) lists them all;
[internal/config/config.go](internal/config/config.go) documents each one.

The defaults copy allsky's settings for this camera: 5 s at gain 50 at night,
one frame about every 20 seconds.

## Deploy to the Pi

The first deploy provisions the Pi, and later deploys update it.

1. Flash Raspberry Pi OS Lite (64-bit) to a new SD card with Raspberry Pi
   Imager. Set the hostname to `allsky` and enable SSH with your key.
2. Swap the card into the Pi. Keep the allsky card: it's the rollback.
3. From this repo, run `.\scripts\pi\deploy-pi.ps1 -Target <user>@allsky.local`.
   The script builds `skycam` for arm64, copies it to the Pi with the ZWO SDK
   and udev rule, and runs [scripts/pi/setup.sh](scripts/pi/setup.sh).
4. On the Pi, edit `/etc/skycam/config.json`: set `images_password` to the
   password in skyq's config, then the relay token and S3 keys.
5. Run `sudo systemctl restart skycam`, then open http://allsky.local/.

After the switch, run `skyq calibrate` on the NUC. skycam's auto-exposure
changes the brightness levels skyq's cloud threshold was tuned on.

## Deploy the relay

`skycam-web` ships like the other Linode apps: a push to `main` runs
[.github/workflows/deploy.yml](.github/workflows/deploy.yml). Provision it once:

1. Create the A and AAAA records for skycam.mchugh.au (DNS only).
2. On the Linode, run:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/exploded/skycam/main/scripts/server-setup.sh | sudo bash
   ```

3. Fill in `/var/www/skycam/.env`: `SKYCAM_TOKEN` (the Pi's `relay.token`),
   `SKYCAM_PASSWORD` and `MONITOR_API_KEY`.
4. Add the printed Caddy block to `/etc/caddy/Caddyfile`, then run
   `sudo systemctl reload caddy`.
5. Push to `main` and watch the deploy.
6. In monitor, add an uptime check on https://skycam.mchugh.au/health/pi. It
   fails when the Pi hasn't been heard from for 10 minutes.

The relay writes night reports to `/var/www/deepspaceplace/reports/skycam/`,
which deepspaceplace serves at https://www.deepspaceplace.com/reports/skycam/.

## How the live view works

1. A viewer opens `/live` on the Pi or on the relay.
2. The Pi learns of a relay viewer through a long poll on `/api/demand`.
3. The Pi switches the camera to video mode, then sends about two frames a
   second.
4. Live mode stops about a minute after the last viewer leaves.

Live mode pauses still capture. At night, a session is capped at 10 minutes
and then blocked for 30, so the timelapse keeps its frames.

## Tests

Run `go test ./...`.

To check star detection on real frames, set `SKYCAM_FRAMES` to a night folder
and run `go test ./internal/analysis -run Sweep -v`.

## Roadmap

- **Native USB driver:** a pure-Go driver for the ASI120MM Mini over Linux
  usbfs, to replace the ZWO SDK (`camera.driver: "native"`).
- **Meteor detection:** find streaks against a rolling median of neighbouring
  frames, and add them to the night report.
