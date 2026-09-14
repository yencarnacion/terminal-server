# Terminal Server

A small Go BYOS server for **TRMNL X**, showing RSS top news, random cowsay fortunes, a month calendar, quarter progress, San Juan weather, newspaper front pages, and configurable Polymarket and Kalshi pages, **one minute per slide** by default. It renders native **1872 × 1404, 4-bit indexed grayscale PNGs** with embedded fonts. No browser, ImageMagick, database, `fortune`, or `cowsay` installation is needed.

The binary includes the 255 quotes from the original `custom_fortunes/my_quotes.txt`. Fortune image URLs stay tied to the exact quote. The calendar takes its inspiration from a paper wall calendar: a bold month/year band, Sunday-first ruled grid, adjacent-month references, a prominent full date, today's cell highlighted in black, and a large time display.

Calendar dates and times default to **America/New_York**, including daylight-saving changes. Override with `--timezone Europe/London` or another IANA zone. Timezone data is embedded, so the server does not depend on the host's timezone database. The displayed clock is **time at refresh**, not a continuously ticking clock. With two newspaper covers and the default interval, the six-slide cycle takes 6 minutes, so the calendar normally gets a fresh timestamp every 6 minutes.

Every device screen has a small bottom-center battery icon and percentage. It adds “Charging” when the device reports charging, or “Power connected” when USB power is reported without charging. At 20% or lower while off external power, the indicator turns black and adds “LOW”; otherwise it uses a quieter gray. Readings update at screen refresh, just like the clock.

Battery telemetry comes from the current device request: `PERCENT_CHARGED` takes priority, with `BATTERY_CAPACITY` (remaining/full) as a fallback. Both hyphenated and underscored firmware headers are supported. Missing or invalid measurements show `Battery —`, including browser previews that have no device telemetry. Voltage alone is not converted into a guessed percentage. Each image URL captures its battery status, so caches and other devices cannot change that reading later.

## Install on 10.17.17.90

Requires Go 1.24 or newer to build. The resulting binary has no runtime dependencies.

```sh
git clone https://github.com/yencarnacion/terminal-server.git
cd terminal-server
go test ./...
go build -o bin/terminal-server .
./bin/terminal-server
```

Open **http://10.17.17.90:8177/preview** for an automatically refreshing browser slideshow. Use **Calendar** or **Fortune** to preview either screen directly, or open `/preview?screen=calendar` and `/preview?screen=cowsay`. Browser previews never advance the device's playlist.

The default is `--screen slideshow --refresh 60`. Each successful device display request advances available RSS news → weather → calendar → quarter progress → fortune → each available newspaper cover → each configured Polymarket page → each configured Kalshi page → weather, independently per device. A center touchbar tap also advances the slideshow. Restarting the server starts the device sequence at the first available configured slide. Use `--screen cowsay`, `--screen calendar`, or `--screen quarter` to show just one screen, and `--refresh` to change seconds per screen. Browser slideshow selection uses wall-clock slots and may be on a different slide from the device. Calendar image URLs preserve the scheduled minute even when downloaded after midnight.

## Top News

`rss_url` in `config.yaml` defaults to `http://10.17.17.98:8090/up2date/rss.xml`; set it to an empty string to disable news. The `rss` slide is first in the default order. The feed is checked at startup and every minute; empty, malformed, or unavailable feeds are skipped automatically and reappear after recovery.

Headlines use the same embedded monospace font at the quote renderer's largest size (64 points). Each item has a prominent QR code for its article URL, with a white quiet zone and crisp integer-sized modules. Only complete rows that fit are shown, in feed order; headlines and QR codes are never shrunk to squeeze in more items. Items without usable HTTP(S) links, or with URLs too dense for a scannable code, are skipped. News is rendered from the latest successful poll without an image cache.

Preview with `/preview?screen=rss`, or use `screen: rss` for news only. The direct preview shows an empty-state message when news is unavailable. Restart after changing configuration.

## Configuration

Slide order is configurable in `config.yaml`:

```yaml
slide_order: [rss, weather, calendar, quarter, quote, newspapers, polymarket, kalshi]
```

Reorder entries or omit screens you do not want, then restart the server. `quote` is the cowsay fortune screen; `newspapers` expands to one slide per discovered newspaper in the source server's catalog order (currently NYT, then El Nuevo Día). `polymarket` expands to the configured event/market pages in their list order; blank URL slots are skipped. `kalshi` expands to the configured Kalshi pages after Polymarket by default. This includes all current screen types. Disabled weather and unavailable newspaper entries are skipped. If no configured slides are available, fortune is the temporary fallback. Empty lists, duplicate entries, and unknown names are rejected. This setting controls both the device and browser slideshow; single-screen mode ignores the order. Each expanded slide uses `slide_seconds`.

The server reads **`config.yaml`** from its working directory at startup. To change the time between slides, edit:

```yaml
slide_seconds: 300 # Five minutes per slide; default 60 (one minute).
```

Restart the server after changing the file. **No rebuild is needed for configuration edits.** The TRMNL learns the new interval on its next request; tap the center touchbar to apply it sooner. This value sets the duration of every slide and the browser preview refresh interval. The minimum is 60 seconds.

The shipped `config.yaml` includes these general settings (weather settings are documented below):

```yaml
slide_seconds: 60
listen: ":8177"
base_url: "http://10.17.17.90:8177"
screen: slideshow
timezone: America/New_York
quotes_file: ""
data_dir: ./data
frontpages_url: "http://10.17.17.90:8100"
```

`screen` accepts `slideshow`, `cowsay`, `calendar`, `quarter`, or `weather`. Empty `quotes_file` uses bundled quotes; empty `frontpages_url` disables newspapers. Cover images are still fetched live and never cached.

Settings precedence is **built-in defaults → YAML → explicitly supplied CLI flags**. Existing flags continue working, so `--refresh 180` overrides `slide_seconds` in the file. Remove explicit flags from your service/launch command for settings you want to control through YAML.

```sh
./bin/terminal-server --config /path/to/config.yaml
./bin/terminal-server --config config.yaml --refresh 600
./bin/terminal-server --config '' # Ignore YAML; use defaults and CLI flags.
```

An absent default `config.yaml` uses the built-in defaults. An explicitly requested missing config, invalid YAML, duplicate/unknown keys, unsupported settings, or multiple YAML documents fails at startup with an error. Relative file paths (including `quotes_file` and `data_dir`) remain relative to the server's working directory, not the YAML file's directory. Keep `data_dir` pointing to the existing device key when changing configuration. Startup logs show the effective interval and timezone.

## Quarter progress

By default, the quarter-progress slide appears immediately after the calendar. Inspired by [TRMNL's Quarter Progress recipe](https://trmnl.com/recipes/212805), it displays one square per day of the current calendar quarter, large completed/remaining counts, and a percentage.

Completed local dates are filled dark, today is outlined with its date number, and future dates are light. **Days left includes today**; the percentage counts only fully completed days. On the first day it is 0%, and at the next quarter's local midnight it resets automatically. Calculations use the configured `--timezone` and count calendar dates, so leap years and DST do not introduce off-by-one errors.

Preview with `/preview?screen=quarter`, or run only this screen using `--screen quarter`. Like the calendar, it reflects the timestamp of its last refresh and includes the shared battery footer.

## San Juan weather

See the Polymarket section below for optional prediction-market slides after newspapers.

Weather appears first by default. Inspired by the [Daily Weather recipe](https://trmnl.com/recipes/150460), it shows the full date, Puerto Rico local time (AST year-round), a large **forecast** temperature in Fahrenheit, rain chance, wind in mph, a short outlook, and five daily high/night-low cards. The battery footer remains visible. This is a forecast, not a live thermometer or emergency alert system.

Data comes directly from the free, public [NOAA/NWS API](https://www.weather.gov/documentation/services-web-api), using the San Juan forecast office's grid forecasts. No API key or subscription is needed. The server resolves coordinates via `/points`, then requests the returned forecast URL with `units=us`. It identifies itself with a User-Agent and checks all URLs/redirects stay on api.weather.gov. It does not scrape the weather.gov website. UV index is omitted because this NWS forecast endpoint does not supply it.

These additional `config.yaml` settings default to San Juan:

```yaml
weather_enabled: true
weather_location: "San Juan, PR"
weather_latitude: 18.4655
weather_longitude: -66.1057
```

Restart after editing. Changing the name alone does not change the forecast; coordinates select the NWS grid. Weather dates always use `America/Puerto_Rico`, independently of the calendar timezone. Set `weather_enabled: false` to remove it, or `screen: weather` for weather only. Preview at `/preview?screen=weather`.

Forecast data is shared in memory for 15 minutes to respect NWS rate limits; rendered weather images are not cached. Newspaper covers remain completely uncached. Each weather image shows its render time and NWS issue time. On fetch failure, the slide displays an unavailable message and retries after two minutes, without interrupting other slides or silently using stale data. Forecasts issued over 24 hours ago are rejected. Missing values show a dash, not zero. Today's high disappears after its forecast period ends; night lows belong to the evening when that period starts. Daily rain chance is the maximum of the remaining day/night period probabilities, not a calculated whole-day probability.

## Polymarket pages

Inspired by the [TRMNL Polymarket recipe](https://trmnl.com/recipes/186483), each configured page becomes a separate full-screen slide after newspapers, using the shared `slide_seconds` duration (60 seconds by default), date/time, and battery footer. This is a read-only display; it never connects a wallet or places trades.

Paste up to three page URLs into `config.yaml`, then restart:

```yaml
slide_order: [rss, weather, calendar, quarter, quote, newspapers, polymarket, kalshi]
polymarket_pages:
  - "https://polymarket.com/event/what-price-will-bitcoin-hit-before-2027"
  - "https://polymarket.com/event/which-party-will-win-the-house-in-2026"
  - "https://polymarket.com/event/which-party-will-win-the-senate-in-2026"
```

The initial pages are Bitcoin, House, and Senate, in that order. Replace these URLs to choose different pages; blank slots are skipped. The server accepts `/event/event-slug`, `/event/event-slug/market-slug` for a specific market in an event, and `/market/market-slug` URLs on polymarket.com. Category, profile, and search pages are not supported. Tracking query parameters are ignored. Duplicate pages, unsupported URLs, and more than three slots are rejected at startup. Browser preview navigation gains a link for each configured page. If you already have a custom `slide_order`, add `polymarket` after `newspapers` yourself.

The server fetches the public [Polymarket Gamma API](https://docs.polymarket.com/api-reference/events/get-event-by-slug) on every image request, with no API key, local data cache, or image cache. Requests have an 18-second timeout, a 4 MiB response limit, and redirects restricted to the API host. API errors produce an unavailable slide and retry on the next display rather than showing saved prices.

Single-market pages show each outcome; multi-market events show the Yes price for each binary market (or each named outcome for nonbinary markets). Up to six rows fit on a screen, with a visible shown/total count. Open markets sort first, then descending outcome price. Archived markets are omitted; closed/inactive markets are labeled. Missing or invalid prices show a dash. Percentages are reported market prices, not guaranteed probabilities or a complete order-book quote; fetch time is not the time of the last trade. This version does not include price-history charts.

## Kalshi pages

Four Kalshi slides follow Polymarket: Bitcoin's end-of-2026 price range, Congress balance of power, the top US Netflix movie, and the #2 US Netflix show. They share the prediction-market layout, date/time, battery footer, and configured slide duration (300 seconds in the checked-in configuration). Each has a browser preview link.

```yaml
kalshi_pages:
  - "https://kalshi.com/markets/kxbtcy/btc-price-range-eoy/kxbtcy-27jan0100"
  - "https://kalshi.com/markets/kxbalancepowercombo/congress-balance-of-power-combo/kxbalancepowercombo-27feb"
  - "series:KXNETFLIXRANKMOVIE"
  - "series:KXNETFLIXRANKSHOWRUNNERUP"
```

Event URLs select fixed events. A `series:TICKER` entry discovers open events on every display and selects the nearest closing event with an active, already-open market. The Netflix slides therefore roll forward automatically each week; the event subtitle identifies the chart publication date. When no open week is available, the slide displays an unavailable message and retries next time. It does not reuse a settled week's prices. Up to four entries are supported; blanks are skipped and duplicates rejected. Use `kalshi_pages: []` or omit `kalshi` from `slide_order` to disable them.

The public [Kalshi market-data API](https://docs.kalshi.com/getting_started/quick_start_market_data) supplies fresh data without an API key. Requests have an 18-second total deadline, a 4 MiB per-response limit, restricted redirects, and no data/image cache. Weekly discovery follows [event-list pagination](https://docs.kalshi.com/api-reference/events/get-events).

Slides show the six highest last-trade prices, with open markets first, and report the total outcome count. Prices are expressed as percentages; settled outcomes use their Yes/No result. Missing, invalid, and untraded prices display a dash. Restart the server after changing configuration.

## Newspaper front pages

By default the server discovers newspapers from **http://10.17.17.90:8100/api/newspapers**, then fetches each paper's `/api/newspapers/{id}/today` metadata and its `image_url`. Currently the service provides **The New York Times** and **El Nuevo Día**, giving a six-slide cycle with 60 seconds for each slide by default. Additional papers appear automatically in service catalog order.

Set `--frontpages-url http://host:port` to change the source, or `--frontpages-url ''` to disable newspaper slides. The source is your [frontpages service](https://github.com/yencarnacion/frontpages); no API key is needed in Terminal Server. Fetching `/today` may trigger the source service's normal scrape/cache behavior. Terminal Server does not call admin or force-refresh endpoints.

**Newspaper covers are never cached by Terminal Server.** Every cover-image request calls that paper's `/today` endpoint on port 8100, downloads the returned `image_url`, and renders it afresh. The cover is not retained on disk or in the rendered-image cache. Upstream requests send no-cache headers; downstream PNG responses send `Cache-Control: no-store, no-cache, max-age=0`. Every scheduled cover also has a unique timestamped filename to prevent the TRMNL from reusing yesterday's image. Even a repeated request to an old cover URL fetches `/today` again. Port 8100 remains responsible for obtaining the day's source edition; Terminal Server displays its reported **edition date** rather than assuming it matches today's date.

Only the list of configured newspaper names/IDs is kept and refreshed every 15 minutes. Until the first catalog response, fortune, calendar, quarter progress, and weather continue. Removed papers disappear on the next successful catalog refresh. If a cover fetch fails, the image request fails rather than serving a saved edition. Fortune, calendar, and quarter progress remain available. A catalog outage preserves the newspaper list, not any cover images.

Each cover gets its own full-screen grayscale slide and the shared battery footer. **The New York Times** (`ny_nyt-The_New_York_Times`) is cropped to the **upper-right quadrant** of its source spread and enlarged proportionally, showing the masthead and top stories like a folded newspaper at a newsstand. **The Wall Street Journal** (`wsj-The_Wall_Street_Journal`) and **San Francisco Chronicle** (`ca_sfc-San_Francisco_Chronicle`) are cropped to their **upper halves**, preserving the full page width and enlarging the masthead and lead stories. **El Nuevo Día and other papers remain uncropped**, fitted proportionally. Source resolution limits readability; the server cannot add detail missing from a small source image. The current El Nuevo Día image is 468 × 492 pixels. Network timeouts, response-size limits, and an image-pixel limit bound failures. Cover URLs and redirects must stay on the configured frontpages origin.

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
ExecStart=/absolute/path/terminal-server/bin/terminal-server --config /absolute/path/terminal-server/config.yaml
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
