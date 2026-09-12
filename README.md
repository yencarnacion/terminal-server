# Terminal Server

A small Go BYOS server for **TRMNL X**, showing random cowsay fortunes, a month calendar, quarter progress, and newspaper front pages, **three minutes per slide**. It renders native **1872 × 1404, 4-bit indexed grayscale PNGs** with embedded fonts. No browser, ImageMagick, database, `fortune`, or `cowsay` installation is needed.

The binary includes the 255 quotes from the original `custom_fortunes/my_quotes.txt`. Fortune image URLs stay tied to the exact quote. The calendar takes its inspiration from a paper wall calendar: a bold month/year band, Sunday-first ruled grid, adjacent-month references, a prominent full date, today's cell highlighted in black, and a large time display.

Calendar dates and times default to **America/New_York**, including daylight-saving changes. Override with `--timezone Europe/London` or another IANA zone. Timezone data is embedded, so the server does not depend on the host's timezone database. The displayed clock is **time at refresh**, not a continuously ticking clock. With two newspaper covers, the five-slide cycle takes 15 minutes, so the calendar normally gets a fresh timestamp every 15 minutes.

Every device screen has a small bottom-center battery icon and percentage. It adds “Charging” when the device reports charging, or “Power connected” when USB power is reported without charging. At 20% or lower while off external power, the indicator turns black and adds “LOW”; otherwise it uses a quieter gray. Readings update at screen refresh, just like the clock.

Battery telemetry comes from the current device request: `PERCENT_CHARGED` takes priority, with `BATTERY_CAPACITY` (remaining/full) as a fallback. Both hyphenated and underscored firmware headers are supported. Missing or invalid measurements show `Battery —`, including browser previews that have no device telemetry. Voltage alone is not converted into a guessed percentage. Each image URL captures its battery status, so caches and other devices cannot change that reading later.

## Install on 10.17.17.90

Requires Go 1.24 or newer to build. The resulting binary has no runtime dependencies.

```sh
git clone https://github.com/yencarnacion/terminal-server.git
cd terminal-server
go test ./...
go build -o bin/terminal-server .
./bin/terminal-server \
  --listen :8177 \
  --base-url http://10.17.17.90:8177 \
  --data-dir ./data
```

Open **http://10.17.17.90:8177/preview** for an automatically refreshing browser slideshow. Use **Calendar** or **Fortune** to preview either screen directly, or open `/preview?screen=calendar` and `/preview?screen=cowsay`. Browser previews never advance the device's playlist.

The default is `--screen slideshow --refresh 180`. Each successful device display request advances fortune → calendar → quarter progress → each available newspaper cover → fortune, independently per device. A center touchbar tap also advances the slideshow. Restarting the server starts the device sequence at fortune again. Use `--screen cowsay`, `--screen calendar`, or `--screen quarter` to show just one screen, and `--refresh` to change seconds per screen. Browser slideshow selection uses wall-clock slots and may be on a different slide from the device. Calendar image URLs preserve the scheduled minute even when downloaded after midnight.

## Quarter progress

The quarter-progress slide appears immediately after the calendar. Inspired by [TRMNL's Quarter Progress recipe](https://trmnl.com/recipes/212805), it displays one square per day of the current calendar quarter, large completed/remaining counts, and a percentage.

Completed local dates are filled dark, today is outlined with its date number, and future dates are light. **Days left includes today**; the percentage counts only fully completed days. On the first day it is 0%, and at the next quarter's local midnight it resets automatically. Calculations use the configured `--timezone` and count calendar dates, so leap years and DST do not introduce off-by-one errors.

Preview with `/preview?screen=quarter`, or run only this screen using `--screen quarter`. Like the calendar, it reflects the timestamp of its last refresh and includes the shared battery footer.

## Newspaper front pages

By default the server discovers newspapers from **http://10.17.17.90:8100/api/newspapers**, then fetches each paper's `/api/newspapers/{id}/today` metadata and its `image_url`. Currently the service provides **The New York Times** and **El Nuevo Día**, giving a five-slide cycle with 180 seconds for each slide. Additional papers appear automatically in service catalog order.

Set `--frontpages-url http://host:port` to change the source, or `--frontpages-url ''` to disable newspaper slides. The source is your [frontpages service](https://github.com/yencarnacion/frontpages); no API key is needed in Terminal Server. Fetching `/today` may trigger the source service's normal scrape/cache behavior. Terminal Server does not call admin or force-refresh endpoints.

**Newspaper covers are never cached by Terminal Server.** Every cover-image request calls that paper's `/today` endpoint on port 8100, downloads the returned `image_url`, and renders it afresh. The cover is not retained on disk or in the rendered-image cache. Upstream requests send no-cache headers; downstream PNG responses send `Cache-Control: no-store, no-cache, max-age=0`. Every scheduled cover also has a unique timestamped filename to prevent the TRMNL from reusing yesterday's image. Even a repeated request to an old cover URL fetches `/today` again. Port 8100 remains responsible for obtaining the day's source edition; Terminal Server displays its reported **edition date** rather than assuming it matches today's date.

Only the list of configured newspaper names/IDs is kept and refreshed every 15 minutes. Until the first catalog response, fortune, calendar, and quarter progress continue alone. Removed papers disappear on the next successful catalog refresh. If a cover fetch fails, the image request fails rather than serving a saved edition. Fortune, calendar, and quarter progress remain available. A catalog outage preserves the newspaper list, not any cover images.

Each cover gets its own full-screen grayscale slide and the shared battery footer. **The New York Times** (`ny_nyt-The_New_York_Times`) is cropped to the **upper-right quadrant** of its source spread and enlarged proportionally, showing the masthead and top stories like a folded newspaper at a newsstand. **El Nuevo Día and other papers remain uncropped**, fitted proportionally. Source resolution limits readability; the server cannot add detail missing from a small source image. The current El Nuevo Día image is 468 × 492 pixels. Network timeouts, response-size limits, and an image-pixel limit bound failures. Cover URLs and redirects must stay on the configured frontpages origin.

Open `/preview` and select either newspaper's name to inspect its slide. `/healthz` includes the number of discovered `covers` (configured newspaper slides, not a guarantee that their upstream images are currently available).

## Upgrade an existing installation

Stop the currently running server (Ctrl-C if running in a terminal), then run:

```sh
git pull --ff-only
go test ./...
go build -o bin/terminal-server .
./bin/terminal-server
```

If using systemd, stop/start `terminal-server` with `systemctl --user stop terminal-server` and `systemctl --user start terminal-server` instead of launching it manually. Preserve the existing `data` directory and working directory. Remove old explicit `--screen cowsay` or `--refresh 600` arguments from your launch command/service so the new defaults apply. No TRMNL re-pairing is needed. Tap the center touchbar once to fetch the new three-minute refresh setting, or wait for the next scheduled wake.

Port 8177 refused a TCP connection during development on September 12, 2026, so it was selected outside the excluded 8080–8099 range. A refused connection is not a permanent reservation or a guarantee against firewall rejection: confirm on the target host with `ss -ltn` (Linux) or `lsof -nP -iTCP:8177 -sTCP:LISTEN` (macOS) before starting. If needed, change both `--listen` and `--base-url`. Allow inbound TCP on the selected port from the device's LAN.

To use a different fortune file:

```sh
./bin/terminal-server --quotes ~/custom_fortunes/my_quotes.txt
```

Separate quotes with a line containing `%`. Files are read on startup; restart after editing. No `.dat` file is used. With no `--quotes`, rebuild to incorporate edits to `quotes.txt`.

## Connect the TRMNL X

In the device's Wi-Fi setup portal, choose **Advanced → Custom Server → Yes** and enter:

```text
http://10.17.17.90:8177
```

Do not include a trailing slash. Complete Wi-Fi setup. The device provisions itself through `/api/setup`, receives a stable API key, then polls `/api/display` and downloads its PNG. Physical-device testing is still required after installation; the repository tests simulate these HTTP requests.

Keep `data/device-key` across upgrades and service restarts: it signs the API keys issued to devices. It is generated with mode 0600 and excluded from Git. Losing it requires device reprovisioning. Setup and previews are open to the LAN; display and device-log requests require the issued `ACCESS_TOKEN`. This is intended for a trusted LAN, not a public Internet deployment. No firmware updates or resets are requested.

## Run continuously on Linux

After cloning and building under your account, create a user service at `~/.config/systemd/user/terminal-server.service` using the example below. Replace `/absolute/path/terminal-server` with your checkout's absolute path in both places.

```ini
[Unit]
Description=TRMNL X Terminal Server
After=network-online.target

[Service]
WorkingDirectory=/absolute/path/terminal-server
ExecStart=/absolute/path/terminal-server/bin/terminal-server --listen :8177 --base-url http://10.17.17.90:8177 --data-dir ./data
Restart=on-failure
RestartSec=5
UMask=0077

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now terminal-server
journalctl --user -u terminal-server -f
```

For startup at boot without logging in, enable lingering for the account with `sudo loginctl enable-linger "$USER"`. On macOS, run the same binary through your preferred service manager or directly in a terminal.

## Endpoints and development

| Route | Purpose |
| --- | --- |
| `GET /preview` | Browser slideshow; optional `?screen=calendar`, `?screen=quarter`, or `?screen=cowsay` |
| `GET /healthz` | Health, quote count and resolution |
| `GET /api/setup` | Provision using device MAC in `ID` |
| `GET /api/display` | Next slideshow screen; requires `ACCESS_TOKEN` |
| `POST /api/log` | Accept device JSON logs, up to 64 KiB; requires `ACCESS_TOKEN` |
| `GET /screens/{id}.png` | Stable PNG for a quote, captured calendar/quarter minute, or live newspaper cover |

`Screen` in `render.go` is the renderer interface; `calendar.go` implements the calendar, `quarter.go` implements quarter progress, and `frontpages.go` fetches/renders newspaper covers. `screenID` selects the image input and `nextDisplay` controls the playlist. Fortune/calendar/quarter rendering uses a bounded, concurrency-safe cache; newspaper images bypass it entirely. Tests cover date boundaries, independent device cursors, the five-slide sequence, battery status, live fetching on every cover request, and changing editions without reusing old image data.

```sh
go test -race ./...
go vet ./...
go build -o bin/terminal-server .
```

Protocol and hardware references: [TRMNL BYOS](https://docs.trmnl.com/go/diy/byos), [firmware API](https://github.com/usetrmnl/terminus/blob/main/doc/api.adoc), [TRMNL X specifications](https://enterprise.trmnl.app/products/x/spec-sheet), [custom-server setup](https://help.trmnl.com/en/articles/12263392-connect-your-device-to-terminus-byos).
