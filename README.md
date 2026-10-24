# pos-ticker

POS ticker tape for your desk: web form submissions print instantly on any ESC/POS thermal printer.

Visitors submit your site's contact form; the message prints on a USB or Ethernet thermal printer on your desk. One static Go binary, a tiny `FROM scratch` image, an interactive admin UI, and a strict split between the **public** contact-form listener and the **private** admin listener.

---

## Features

- **Single static binary** (pure Go, no cgo, no libusb), also shipped as a `FROM scratch` multi-arch image (amd64, arm64, arm/v7). Release binaries are about 14 MB.
- **Two listeners, by design**
  - *Public* (`PUBLIC_ADDR`): only `POST /contact` and `GET /contact/count`. Safe to put behind a Cloudflare Tunnel.
  - *Admin* (`ADMIN_ADDR`): admin UI, API, `/metrics`. LAN / Tailscale only; never routed through the tunnel.
- **USB or Ethernet printers**: Ethernet-first with USB fallback (or force either). USB goes through the kernel `usblp` driver (`/dev/usb/lp*`).
- **Live admin UI** at `/admin`: queue, searchable message history, print/skip, image printing with live receipt preview, settings editor, keyboard shortcuts. Live updates over SSE.
- **Emoji and non-ASCII** text rendered to a bitmap with bundled monochrome fonts.
- **Rate limiting**: global prints/hour plus per-IP cap, editable at runtime.
- **Gmail notifications** on every new submission (optional).
- **SQLite persistence**, WAL mode. Existing `messages.db` files from the Python version work as-is.
- **Optional observability** (compose profile): Prometheus, node-exporter and an OpenTelemetry collector that *scrapes* the app's hand-written `/metrics`. Nothing starts by default and the app has no OpenTelemetry SDK.
- **Supply chain**: images are scanned with Trivy, signed with cosign (keyless) and published with SBOM and provenance attestations; release binaries come with checksums.

---

## Architecture

```
                               your website (browser)
                                        |
                                        |  POST /contact
                                        v
                          +----------------------------+
                          |      Cloudflare edge       |
                          +----------------------------+
                                        |  Cloudflare Tunnel (outbound from your host)
                                        v
 +------------------ host (Raspberry Pi / server) -----------------------------+
 |                                                                              |
 |   cloudflared  (compose profile "tunnel")                                    |
 |        |   hostname -> http://printer:8080                                   |
 |        v                                                                     |
 |   +------------------------- pos-ticker ----------------------------+       |
 |   |  PUBLIC listener  :8080     POST /contact, GET /contact/count    |       |
 |   |                             everything else -> 404               |       |
 |   |  ------------------------------------------------------------   |       |
 |   |  ADMIN listener   :8000     /admin  /api  /events  /metrics      |<--- LAN / Tailscale
 |   |                             key or session cookie required;      |    (browser, Prometheus)
 |   |                             404s anything carrying CF-Ray        |       |
 |   |                                                                  |       |
 |   |  queue -> render -> ESC/POS ---+--- TCP :9100  (Ethernet printer)|       |
 |   |  SQLite (/data/messages.db)    +--- /dev/usb/lp0 (usblp, USB)    |       |
 |   +------------------------------------------------------------------+       |
 +------------------------------------------------------------------------------+
```

Only the public listener is ever meant to be reachable from the internet, and only through the tunnel. The admin listener binds loopback by default.

---

## Requirements

| Item | Notes |
|---|---|
| Linux host | Raspberry Pi 4/5 (arm64), Pi 3 (arm/v7) or any amd64 box |
| Docker + Compose plugin **or** systemd | pick one install method below |
| ESC/POS thermal printer | USB **or** Ethernet, 58 mm or 80 mm roll |
| [Tailscale](https://tailscale.com) | recommended for remote access to the admin UI |

---

## Install

### Option A: Docker Compose (recommended)

```bash
git clone https://github.com/badhrinadhbade/pos-ticker.git
cd pos-ticker
./setup.sh          # choose "1) Docker Compose"
```

`setup.sh` detects your printer, loads the `usblp` module, installs a udev rule, asks about the optional profiles, generates an `ADMIN_KEY` if you leave it blank, writes a `chmod 600` `.env`, then pulls the image and starts the stack.

Manual equivalent:

```bash
cp .env.example .env && chmod 600 .env
$EDITOR .env                      # set ADMIN_KEY (>= 16 chars), ALLOWED_ORIGIN, printer settings

docker compose up -d                                              # network printer
docker compose -f docker-compose.yml -f docker-compose.usb.yml up -d   # USB printer
```

Open `http://127.0.0.1:8000/admin` and sign in with `ADMIN_KEY`.

**What runs by default: one service, `printer`**, hardened: read-only root filesystem, all capabilities dropped, `no-new-privileges`, non-root uid 65532, `/tmp` on tmpfs, data in the named volume `printer_data` mounted at `/data`. Ports are published on loopback only:

| Host port | Listener | Change with |
|---|---|---|
| `127.0.0.1:8000` | admin | `ADMIN_BIND` in `.env` |
| `127.0.0.1:8080` | public contact form | `PUBLIC_BIND` in `.env` |

**Tailscale access to the admin UI:** set `ADMIN_BIND=<tailscale ip>` (`tailscale ip -4`) in `.env` and `docker compose up -d`.

**Why USB is a separate override file.** Docker refuses to start a container whose `devices:` entry does not exist, which would break network-printer users and hosts with the printer unplugged. So the base file passes no device, and `docker-compose.usb.yml` adds exactly one node (`${PRINTER_USB_DEVICE:-/dev/usb/lp0}`) plus the host `lp` group. Enable it per command with `-f`, or permanently with `COMPOSE_FILE=docker-compose.yml:docker-compose.usb.yml` in `.env` (`setup.sh` does this when it finds a device).

#### Profile `tunnel`: Cloudflare Tunnel for the contact form

```bash
# .env: TUNNEL_TOKEN=...   TRUST_PROXY=true
docker compose --profile tunnel up -d
```

1. Zero Trust dashboard -> Networks -> Tunnels -> create a tunnel, copy its token into `TUNNEL_TOKEN`.
2. Add a **Public Hostname** `contact.example.com` -> service **`http://printer:8080`**.
3. Do **not** add a hostname for port 8000. The admin listener is meant for LAN/Tailscale only.
4. Point your form at `https://contact.example.com/contact`, and set `ALLOWED_ORIGIN` to your site's exact origin.

Set `TRUST_PROXY=true` so per-IP rate limits use `Cf-Connecting-Ip`.

#### Profile `observability`: Prometheus, node-exporter, OpenTelemetry collector

```bash
docker compose --profile observability up -d     # or COMPOSE_PROFILES=observability in .env
```

- Prometheus on `127.0.0.1:9090` scrapes `printer:8000/metrics`, `node-exporter:9100` and the collector (`otel-collector:8888`). It authenticates to `/metrics` with the admin key, which compose writes at start-up to a file on the container's tmpfs (`authorization.credentials_file` in `prometheus.yml`); the key never lands in a host file or in the config.
- The collector scrapes the same endpoint (key passed via its environment) and exports to whatever backend you uncomment in `otel-collector.yml` (Datadog, Grafana Cloud, remote-write...). The app itself pushes no OTLP.
- node-exporter runs on the compose network with read-only host mounts and publishes no port. Per-NIC host network counters need `network_mode: host` (see the comment in `docker-compose.yml`).
- All images are pinned to tags (overridable via `PROMETHEUS_VERSION`, `NODE_EXPORTER_VERSION`, `OTELCOL_VERSION`, `CLOUDFLARED_VERSION`).

### Option B: single binary + systemd (no Docker)

```bash
git clone https://github.com/badhrinadhbade/pos-ticker.git && cd pos-ticker
./setup.sh          # choose "2) Single binary + systemd"
```

This downloads the right release binary (`linux/amd64`, `arm64` or `armv7`) from GitHub Releases, verifies its SHA-256 against `checksums.txt` (and, if `cosign` is installed, the signature on that file; otherwise it warns that only integrity was checked), installs it to `/usr/local/bin/pos-ticker`, creates the `pos-ticker` system user (member of `lp`), writes `/etc/pos-ticker/pos-ticker.env` (mode 600) and enables the hardened `pos-ticker.service` (see `printer.service`: `NoNewPrivileges`, `ProtectSystem=strict`, `StateDirectory`, a closed device policy that only allows `/dev/usb/lp0..3`, syscall and address-family filters).

Manual install:

```bash
ARCH=amd64   # or arm64 / armv7
curl -fsSLO https://github.com/badhrinadhbade/pos-ticker/releases/latest/download/pos-ticker_linux_$ARCH.tar.gz
curl -fsSLO https://github.com/badhrinadhbade/pos-ticker/releases/latest/download/checksums.txt
sha256sum -c --ignore-missing checksums.txt
# authenticity (needs cosign): the checksums are signed by this repo's release workflow
curl -fsSLO https://github.com/badhrinadhbade/pos-ticker/releases/latest/download/checksums.txt.sigstore.json
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/badhrinadhbade/pos-ticker/\.github/workflows/docker-publish\.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
tar xzf pos-ticker_linux_$ARCH.tar.gz

sudo useradd --system --no-create-home --shell /usr/sbin/nologin pos-ticker
sudo install -m 0755 pos-ticker /usr/local/bin/pos-ticker
sudo install -d -m 0750 /etc/pos-ticker
sudo install -m 0600 .env.example /etc/pos-ticker/pos-ticker.env   # set ADMIN_KEY etc.
sudo cp printer.service /etc/systemd/system/pos-ticker.service
sudo systemctl daemon-reload && sudo systemctl enable --now pos-ticker
```

Check the sandbox with `systemd-analyze security pos-ticker`. For a tunnel in this mode, install `cloudflared` yourself and map the hostname to `http://127.0.0.1:8080` only.

### Building from source

```bash
go build -trimpath -ldflags "-s -w" -o pos-ticker ./cmd/pos-ticker      # CGO_ENABLED=0
docker build -t pos-ticker .                                           # or: build: . in compose
docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 -t pos-ticker .
```

---

## USB printers (usblp)

pos-ticker talks to USB printers through the kernel's `usblp` driver, which exposes the printer as `/dev/usb/lp0`. There is no libusb and no raw USB bus passthrough.

```bash
lsmod | grep usblp || sudo modprobe usblp        # load the driver
echo usblp | sudo tee /etc/modules-load.d/usblp.conf   # ...on every boot
ls -l /dev/usb/lp*                               # should show crw-rw---- root lp
```

Permissions come from a udev rule (installed by `setup.sh` as `/etc/udev/rules.d/99-pos-ticker-usblp.rules`):

```
SUBSYSTEM=="usbmisc", KERNEL=="lp[0-9]*", GROUP="lp", MODE="0660"
```

The app picks the device by matching `PRINTER_USB_VENDOR` / `PRINTER_USB_PRODUCT` against `/sys/class/usbmisc/lp*/device/..`; set `PRINTER_USB_DEVICE=/dev/usb/lp1` to force a node. With Docker, set it in `.env` too: that exact node is what gets passed into the container, together with the numeric `lp` gid (`PRINTER_GID`).

### Troubleshooting

| Symptom | Fix |
|---|---|
| No `/dev/usb/lp0` | `sudo modprobe usblp`; replug/power-cycle the printer; `dmesg \| tail` should mention `usblp`. Some printers show up as vendor-specific or serial-over-USB devices that `usblp` does not claim: use their Ethernet port or another mode. |
| `permission denied` opening the device | Install the udev rule, then `sudo udevadm control --reload-rules && sudo udevadm trigger --subsystem-match=usbmisc`. Docker: check `PRINTER_GID` matches `getent group lp`. systemd: the user must be in `lp`. |
| Compose says `error gathering device information` | The device path does not exist. Use the USB override only when the printer is plugged in, or fix `PRINTER_USB_DEVICE`. |
| Device is `lp1`, not `lp0` | Set `PRINTER_USB_DEVICE=/dev/usb/lp1` in `.env`. |
| CUPS grabs the printer | `sudo systemctl disable --now cups cups-browsed`, or point pos-ticker at the CUPS-free node. |
| Printer works over USB but not after reboot | Make sure `/etc/modules-load.d/usblp.conf` exists. |

---

## Configuration

Settings are env vars (see `.env.example`). Most printer, limit and origin settings only seed the SQLite settings table on first run; afterwards use the admin UI (Settings) or `PATCH /settings`.

| Variable | Default | Description |
|---|---|---|
| `ADMIN_KEY` | _(required)_ | Admin secret, minimum 16 characters. The server refuses to start without it |
| `PUBLIC_ADDR` | `127.0.0.1:8080` | Public contact-form listener (binary default; the image sets `0.0.0.0:8080`) |
| `ADMIN_ADDR` | `127.0.0.1:8000` | Admin listener (the image sets `0.0.0.0:8000`; compose controls exposure via `ADMIN_BIND`) |
| `DB_PATH` | `/data/messages.db` (image) | SQLite file |
| `DISPLAY_TZ` | `America/Los_Angeles` | Timezone for receipt timestamps (tzdata is built in) |
| `ALLOWED_ORIGIN` | _none_ | CORS origin for `/contact`; never `*` |
| `TRUST_PROXY` | `false` | Trust `Cf-Connecting-Ip` / `X-Forwarded-For` on the **public** listener, and only from private/loopback peers (see Security model) |
| `ADMIN_ALLOWED_HOSTS` | _empty_ | Extra `Host` values the admin listener accepts, comma-separated (e.g. your Tailscale name) |
| `PRINTER_MODE` | `auto` | `auto` (network, then USB), `usb`, `network` |
| `PRINTER_HOST` / `PRINTER_PORT` | _empty_ / `9100` | Ethernet printer |
| `PRINTER_USB_VENDOR` / `PRINTER_USB_PRODUCT` | `0x0525` / `0xa700` | Match the `usblp` device by IDs (`lsusb`) |
| `PRINTER_USB_DEVICE` | _auto_ | Explicit device node, e.g. `/dev/usb/lp0` |
| `PRINTER_WIDTH_PX` | `384` | 58 mm = 384, 80 mm = 576 |
| `PRINTS_PER_HOUR` / `MAX_PER_IP_HOUR` | `10` / `3` | Global and per-IP caps |
| `LINE_WIDTH` | `32` | Characters per line (32 for 58 mm, 48 for 80 mm) |
| `SMTP_USER`, `SMTP_PASS`, `NOTIFY_EMAIL`, `EMAIL_NOTIFICATIONS` | | Gmail notifications ([app password](https://myaccount.google.com/apppasswords)) |
| `TUNNEL_TOKEN` | | Cloudflare Tunnel token (profile `tunnel`); only the `cloudflared` container receives it |
| `ADMIN_BIND`, `PUBLIC_BIND`, `PRINTER_GID` | `127.0.0.1`, `127.0.0.1`, `7` | Compose-only: host bind addresses and `lp` gid |
| `POS_TICKER_TAG` | `main` | Compose-only: image tag; pin a release and digest, e.g. `v1.0.0@sha256:...` (never `latest`) |

Abuse caps: `MAX_SUBMISSIONS_PER_HOUR`, `MAX_PENDING_ROWS` and `PRINTER_ALLOW_LOOPBACK` (see `.env.example`). Advanced (rarely changed): `MAX_MESSAGE_CHARS` (500), `MAX_IMAGE_BYTES` (10 MiB), `MAX_IMAGE_PIXELS` (24M), `MAX_IMAGE_HEIGHT` (1200), `IMAGE_BAND_PX` (128), `IMAGE_BAND_PAUSE` (0.2 s), `ASCII_LINE_SPACING` (18).

---

## Connecting your contact form

`POST` JSON to the **public** listener (directly, or via your tunnel hostname):

```json
{
  "name":         "Ada Lovelace",
  "email":        "ada@example.com",
  "message":      "Hello from your website!",
  "browser_time": "Mon May 19  3:45 PM"
}
```

`name` and `message` are required (message up to 500 characters), `email` and `browser_time` are optional, and an `image_b64` field may carry an image. Success returns `{"ok":true,"queued":true}`; rate-limited requests get `429`. `GET /contact/count` returns `{"printed": n}`.

---

## API reference

Public listener:

| Method | Path | Description |
|---|---|---|
| `POST` | `/contact` | Submit a message |
| `GET` | `/contact/count` | Number of printed messages |

Admin listener (needs the `pt_session` cookie from `POST /login`, or an `X-Admin-Key` header; `GET /health` is open):

| Method | Path | Description |
|---|---|---|
| `GET` | `/admin` | Admin UI |
| `GET` | `/health` | Health: queue depth, printer, DB |
| `GET` | `/metrics` | Prometheus text metrics (`Authorization: Bearer <key>` or `X-Admin-Key`) |
| `GET` | `/events` | Server-sent events (status, message, print, settings) |
| `GET` | `/queue`, `POST /drain` | Queue stats, re-queue pending |
| `GET` | `/messages` | Paginated history (`limit`, `offset`, `q`) |
| `POST` | `/print/{id}`, `/skip/{id}`, `/print/image` | Print or skip a message, print an image |
| `POST` | `/preview` | Render a receipt preview (PNG) |
| `GET`, `PATCH` | `/settings` | Runtime settings |

---

## Security model

- **Public/admin split.** The internet-facing surface is two endpoints on a separate listener. The admin listener is never given to the tunnel, binds loopback by default, and returns 404 to any request carrying `CF-Ray` or `Cf-Connecting-Ip`.
- **Admin authentication.** Constant-time key comparison, per-IP exponential backoff on failures, the server will not start without a 16+ character key. Browser sessions use an HMAC-signed `HttpOnly; SameSite=Strict` cookie (12 h) with a CSRF header on mutating requests; no key in browser storage.
- **Headers and UI.** Strict CSP without `unsafe-inline` or third-party origins, `nosniff`, `X-Frame-Options: DENY`, `no-referrer`. The UI builds DOM with `textContent`.
- **Untrusted input.** Images are size-capped, dimension-checked before decode, and re-encoded to 1-bit; control characters are stripped from printed text; SQL is parameterised; request bodies and server timeouts are bounded; stored request headers are redacted (`Cookie`, `Authorization`, `X-Admin-Key`).
- **Rate limits.** Global hourly cap and per-IP cap. Set `TRUST_PROXY=true` only behind a proxy you control, otherwise forwarded headers could be spoofed.
- **`TRUST_PROXY` semantics.** It only affects the *public* listener. When true, `Cf-Connecting-Ip` (then the first `X-Forwarded-For` hop) is used as the client IP **only if the TCP peer is a private or loopback address** (the tunnel container, a local reverse proxy); from any other peer the headers are ignored and the socket address is used. The admin listener never trusts forwarded headers.
- **Admin listener is plain HTTP.** It has no TLS of its own, so the admin key and session cookie cross the wire in clear text if you expose it. Keep it on `127.0.0.1` (the default) or reach it over Tailscale (set `ADMIN_BIND` to the host's tailnet IP); never publish it to the internet or route it through a tunnel. `ADMIN_ALLOWED_HOSTS` lists extra `Host` header values it will answer to (for example `pi.tailnet-name.ts.net`), which blocks DNS-rebinding from a browser on your network.
- **Paper and mail limits.** A printed message body is collapsed (runs of blank lines become one) and cut at 40 lines, with a final `...` line, in the text path, the bitmap path and the preview, so one submission cannot spool unbounded paper. E-mail notifications are capped at 20 per rolling hour (excess dropped, one warning logged per hour). Pending submissions and per-hour submissions are bounded by `MAX_PENDING_ROWS` and `MAX_SUBMISSIONS_PER_HOUR`.
- **Runtime hardening.** Docker: `FROM scratch`, uid 65532, read-only rootfs, `cap_drop: ALL`, `no-new-privileges`, only one device node passed through, log rotation, `mem_limit: 256m` / `GOMEMLIMIT=200MiB` and a `pids_limit` on the printer. systemd: `NoNewPrivileges`, `ProtectSystem=strict`, empty capability set, closed device policy.
- **Secrets.** `.env` is `chmod 600` and git-ignored. Compose uses it only to fill in `${VARS}`; each container gets an explicit list of variables, so `TUNNEL_TOKEN` reaches only `cloudflared` and observability tokens (`DD_API_KEY`, `GRAFANA_OTLP_TOKEN`) only the collector. The `printer` container never sees them. Add any new app variable to the `environment:` list of the `printer` service in `docker-compose.yml`.
- **Supply chain.** Trivy gates the build (fixable HIGH/CRITICAL), images are cosign-signed and carry SBOM and provenance; verify with `cosign verify ghcr.io/badhrinadhbade/pos-ticker:<tag> --certificate-identity-regexp 'https://github.com/badhrinadhbade/pos-ticker/' --certificate-oidc-issuer https://token.actions.githubusercontent.com`. For reproducible installs set `POS_TICKER_TAG` to a release tag plus its digest (`v1.0.0@sha256:...`, from `docker buildx imagetools inspect`); compose never uses `:latest`. Every GitHub Action in CI is pinned to a full commit SHA, and Dependabot (weekly) keeps actions, Go modules and Docker base images current. The binary installer verifies the tarball's SHA-256 against `checksums.txt` and, if `cosign` is installed, the signature on `checksums.txt`; without cosign it warns that only integrity, not authenticity, was checked.

Report vulnerabilities privately: see [SECURITY.md](SECURITY.md).

---

## Migrating from the Python version

The SQLite schema is unchanged, so your existing `messages.db` works as-is. All environment variable names are kept; new ones are `PUBLIC_ADDR`, `ADMIN_ADDR`, `DISPLAY_TZ`, `PRINTER_USB_DEVICE`, `TUNNEL_TOKEN` and the compose-only `ADMIN_BIND` / `PUBLIC_BIND` / `PRINTER_GID`. The `OTEL_*` variables are gone. Other differences:

- The old container kept the DB in the volume at `/app/data`; it is now `/data`. Same named volume, same file at its root, but the files must be owned by uid 65532:

  ```bash
  docker compose down
  docker run --rm -v pos-ticker_printer_data:/data busybox chown -R 65532:65532 /data
  docker compose pull && docker compose up -d
  ```

  (The volume name is `<project>_printer_data`; check with `docker volume ls`.) To move a DB to a binary install: copy it to `/var/lib/pos-ticker/messages.db` and `chown pos-ticker:pos-ticker` it.
- USB: the libusb udev rule (`99-thermal-printer.rules`) and `/dev/bus/usb` passthrough are replaced by `usblp` and `/dev/usb/lp*`. Run `setup.sh` again or follow the USB section.
- `/docs` (Swagger) and the OpenTelemetry push are gone; `/metrics` now requires the admin key.
- Admin and public traffic are on different ports (8000 and 8080). Point existing form code/tunnels at the public one.
- Contact messages now allow up to 500 characters.

---

## Running on boot and updating

Docker: containers use `restart: unless-stopped`, so they come back after a reboot once the Docker service starts. Update with `docker compose pull && docker compose up -d`.

Binary: `systemctl enable --now pos-ticker` (done by `setup.sh`). Update by re-running `./setup.sh` option 2 (it keeps your env file) or by replacing `/usr/local/bin/pos-ticker` and `systemctl restart pos-ticker`.

---

## Project structure

```
.
├── cmd/pos-ticker/            # main package (binary entrypoint)
├── internal/                  # store, config, escpos, printer, render, httpapi, notify ...
├── web/                       # admin UI (embedded into the binary)
├── Dockerfile                 # multi-stage -> FROM scratch
├── docker-compose.yml         # printer (+ profiles: tunnel, observability)
├── docker-compose.usb.yml     # USB passthrough override (/dev/usb/lp*)
├── prometheus.yml             # scrape config for the observability profile
├── otel-collector.yml         # collector config (prometheus receiver -> your backend)
├── printer.service            # hardened systemd unit for the single binary
├── setup.sh                   # interactive installer (Docker or binary)
├── .env.example               # copy to .env
└── .github/workflows/docker-publish.yml   # test, scan, build, sign, release
```

Release assets (per tag): `pos-ticker_linux_{amd64,arm64,armv7}.tar.gz`, `checksums.txt`, and a cosign bundle for the checksums.

---

## Contributing

Bug reports, printer compatibility fixes and documentation improvements are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT, see [LICENSE](LICENSE).
