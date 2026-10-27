# skycam

skycam is the observatory's all-sky camera software. It replaces
[allsky](https://github.com/AllskyTeam/allsky) on the Raspberry Pi 4 at
skycam.local, which drives a ZWO ASI120MM Mini mounted inside the roll-off
observatory. The Pi was called `allsky` until 2026-10-27; "allsky" now
means only the old software and the file layout and S3 bucket skycam kept
from it.

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
   http://skycam.local/images/20261023/.
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

## Bench test with the camera on Windows

The SDK driver also runs on 64-bit Windows, so you can test the real camera
on a PC.

1. Install ZWO's camera driver for Windows from
   https://www.zwoastro.com/software/. The SDK can't see the camera without it.
2. Plug the camera in.
3. In your config, set `"camera": {"driver": "sdk", "sdk_path": "<path to the
   SDK's lib\x64\ASICamera2.dll>"}`, and point `listen` at a spare port.
4. Run `go run ./cmd/skycam -config config.json run`, then open the page and
   **Watch live**.

## Configuration

`skycam` reads `/etc/skycam/config.json` on the Pi. Every key has a default,
so the file only needs what differs, and an unknown key is an error.
[config.example.json](config.example.json) lists them all;
[internal/config/config.go](internal/config/config.go) documents each one.

The defaults copy allsky's settings for this camera: 5 s at gain 50 at night,
one frame about every 20 seconds.

### Settings page

http://skycam.local/settings changes the rest of the settings without SSH.
It has no login: it answers only devices on the home network, and the Pi has
no port open to the internet.

- **Save and restart** writes the changes to
  `/var/lib/skycam/settings.json`, which skycam reads over config.json at
  start-up. skycam then exits, and systemd starts it again in about 10
  seconds. Night processing that's under way finishes first.
- settings.json holds only the values that differ from config.json. **Use
  default** puts one setting back; **Reset all to config.json** empties the
  file.
- **Check**, next to the roof, rain and N.I.N.A. addresses, reads the device
  once, so you can test a new address before you save it.
- Passwords, keys, paths, listeners and skyq's 6–30% band stay in
  config.json. The page lists them, with secrets shown only as set or not
  set.
- If settings.json is bad, skycam runs on config.json alone and the page says
  why.

## Deploy to the Pi

The first deploy provisions the Pi, and later deploys update it.

1. Flash Raspberry Pi OS Lite (64-bit) to a new SD card with Raspberry Pi
   Imager. Set the hostname to `skycam` and enable SSH with your key.
2. From this repo, run `.\scripts\pi\deploy-pi.ps1`. It deploys to
   `james@skycam.local`; pass `-Target` for another user or address.
   The script builds `skycam` for arm64, copies it to the Pi with the ZWO SDK
   and udev rule, and runs [scripts/pi/setup.sh](scripts/pi/setup.sh).
3. On the Pi, edit `/etc/skycam/config.json`. Set `latitude` and `longitude`
   to the observatory's, and the `observatory` URLs to the roof, rain sensor
   and N.I.N.A. addresses; the example's values are placeholders. Choose an
   `images_user` and `images_password`, then set the relay token and S3 keys.
   Put the same pair in skyq's config as `skycam_username` and
   `skycam_password`; skyq sends no credentials without them. Leave both empty
   to turn the auth off.
4. Run `sudo systemctl restart skycam`, then open http://skycam.local/.

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

## What runs where

Each machine owns the data it produces:

| Machine | Program | Owns |
|---|---|---|
| Pi | skycam | The sky: capture, cloud, live view, sky report, and the Alpaca weather device. Runs 24x7. |
| NUC | skyq | Imaging: what got through, the transparency index, failure attribution, autofocus and guiding, from the N.I.N.A. and PHD2 logs. |
| Linode | skycam-web | Publishing and remote access |

The NUC reads the sky from the Pi; the Pi never needs the NUC.

## N.I.N.A. weather device

skycam serves an ASCOM Alpaca ObservingConditions device on port 11111 and
answers Alpaca discovery, so it appears in N.I.N.A.'s weather device list.
It is advisory only, never a safety monitor.

| Property | Value |
|---|---|
| `CloudCover` | 0-100, from the star count against the clear-sky baseline |
| `SkyBrightness` | Sky level in ADU per second at unity gain; instrumental, not lux |
| `SkyQuality` | Approximate sky brightness in mag/arcsec², the unit an SQM meter reports; higher is darker |
| `TimeSinceLastUpdate` | Seconds since the newest frame, or since the weather reading for an ambient sensor |
| `Temperature`, `Humidity`, `Pressure`, `DewPoint`, `WindSpeed`, `WindDirection`, `WindGust`, `RainRate` | OpenWeatherMap's current conditions for the site, when `openweathermap.api_key` is set |

The camera values read as unavailable (Alpaca error 1026), never as zero,
when:
- The newest frame is more than 5 minutes old.
- The roof isn't known to be open.
- For `CloudCover`, it's twilight or there's no clear-sky baseline yet.
- For `SkyQuality`, the camera is in day mode.

`SkyQuality` is skycam's sky magnitude plus one calibration offset,
`analysis.sqm_offset`. The default, 0.6, makes a clear, moonless sky read
about 19.2, the published estimate for Donvale. To calibrate it properly,
take an SQM reading (a meter or a phone app) under a clear, moonless sky
with the roof open. Then change the offset by the difference between that
reading and skycam's.

The ambient values come from the OpenWeatherMap Current Weather API. skycam
fetches them every 10 minutes, which is as often as OpenWeatherMap updates,
and serves the cached reading in between. A reading older than 30 minutes
reads as unavailable. OpenWeatherMap's cloud cover is never used. Without an
API key, the ambient sensors are not implemented, and N.I.N.A. greys them out.

Set `alpaca.listen` to `""` to turn the device off.

## Sky API

`GET /api/nights/<YYYYMMDD>/sky` returns one night's frames as JSON: time,
image path, exposure, stars, cloud, sky level, roof and rain. It also
returns the cloud onset: the first of 3 consecutive frames with cloud of 0.5
or more. It uses the same basic auth as `/images/`. skyq reads it to
attribute imaging failures to cloud.

## Tests

Run `go test ./...`.

To check star detection on real frames, set `SKYCAM_FRAMES` to a night folder
and run `go test ./internal/analysis -run Sweep -v`.

## Roadmap

- **Native USB driver:** a pure-Go driver for the ASI120MM Mini over Linux
  usbfs, to replace the ZWO SDK (`camera.driver: "native"`).
- **Meteor detection:** find streaks against a rolling median of neighbouring
  frames, and add them to the night report.
