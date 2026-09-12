# Terminal Server

A small Go BYOS server for **TRMNL X**, starting with random cowsay fortunes. It renders native **1872 × 1404, 4-bit indexed grayscale PNGs** with an embedded monospace font. No browser, ImageMagick, database, `fortune`, or `cowsay` installation is needed.

The binary includes the 255 quotes from the original `custom_fortunes/my_quotes.txt`. Each display request chooses a quote; its image URL stays tied to that exact quote so concurrent requests cannot change the screen being downloaded.

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

Open **http://10.17.17.90:8177/preview** to see a screen. “Another fortune” selects a new random quote. The device refresh interval defaults to 600 seconds; use `--refresh 1800` for 30 minutes.

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
| `GET /preview` | Browser preview with another-fortune link |
| `GET /healthz` | Health, quote count and resolution |
| `GET /api/setup` | Provision using device MAC in `ID` |
| `GET /api/display` | Random screen metadata; requires `ACCESS_TOKEN` |
| `POST /api/log` | Accept device JSON logs, up to 64 KiB; requires `ACCESS_TOKEN` |
| `GET /screens/{id}.png` | Stable PNG for a particular quote |

`Screen` in `render.go` is the renderer interface. Add generators to the registry in `newApp` and select one with `--screen`. Currently only `cowsay` is supported. A playlist is not implemented yet. Rendering uses a bounded, concurrency-safe cache; evicted images are regenerated when needed.

```sh
go test -race ./...
go vet ./...
go build -o bin/terminal-server .
```

Protocol and hardware references: [TRMNL BYOS](https://docs.trmnl.com/go/diy/byos), [firmware API](https://github.com/usetrmnl/terminus/blob/main/doc/api.adoc), [TRMNL X specifications](https://enterprise.trmnl.app/products/x/spec-sheet), [custom-server setup](https://help.trmnl.com/en/articles/12263392-connect-your-device-to-terminus-byos).
