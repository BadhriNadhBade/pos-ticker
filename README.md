# pos-ticker

POS ticker tape for your desk — web form submissions print instantly on any ESC/POS thermal printer.

Website visitors submit a contact form; their message prints immediately on a USB or Ethernet ESC/POS thermal printer sitting on your desk. Includes a web admin UI, Prometheus metrics, and an OpenTelemetry hook for any APM backend.

---

## Features

- **FastAPI + uvicorn** — async, production-grade, auto-docs at `/docs`
- **Dual printer support** — Ethernet-first with USB fallback (or force either)
- **Admin UI** at `/admin` — live queue, message history, runtime settings editor, drain button
- **Prometheus metrics** at `/metrics` — plug into Grafana, Datadog, or any Prometheus-compatible backend
- **OpenTelemetry traces** — configure any OTLP exporter (Datadog, Jaeger, Grafana Cloud) via env var, no code changes
- **Rate limiting** — global prints/hour + per-IP cap, both editable at runtime
- **Emoji support** — messages with emoji render via image fallback at full paper width
- **Gmail notifications** — email on every new submission
- **SQLite persistence** — WAL mode, survives restarts
- **Multi-arch Docker image** — arm64 (Pi 4/5), arm/v7 (Pi 3), amd64

---

## Requirements

| Item | Notes |
|---|---|
| Linux host | Raspberry Pi 4/5 (arm64) recommended; Pi 3 (arm/v7) and amd64 also work |
| Docker + Docker Compose | `apt install docker.io docker-compose-plugin` — or let `setup.sh` install it |
| ESC/POS thermal printer | USB **or** Ethernet, 58 mm or 80 mm roll |
| [Tailscale](https://tailscale.com) | Recommended for secure remote access to `/admin` and `/metrics` |

---

## Quick start

### Automated (recommended)

```bash
git clone https://github.com/badhrinadhbade/pos-ticker.git
cd pos-ticker
chmod +x setup.sh && ./setup.sh
```

`setup.sh` will:
- Install Docker if it isn't already present
- Auto-detect your USB printer's vendor/product IDs
- Ask a few short questions (printer mode, paper width, admin key, optional email)
- Write a locked-down `.env` file
- Install the USB udev permission rule
- Pull the pre-built image and start everything

When it finishes, open **http://localhost:8000/admin**.

---

### Manual setup

<details>
<summary>Click to expand manual steps</summary>

```bash
# 1. Clone
git clone https://github.com/badhrinadhbade/pos-ticker.git
cd pos-ticker

# 2. Find your USB printer IDs (skip for Ethernet)
lsusb | grep -i print
# e.g. "Bus 001 Device 003: ID 0525:a700 Netchip Technology, Inc."
#         vendor ^^^^  product ^^^^

# 3. Configure
cp .env.example .env
nano .env   # fill in ADMIN_KEY, SMTP_*, printer IDs / IP

# 4. Set USB permissions (replace IDs with yours)
echo 'SUBSYSTEM=="usb", ATTR{idVendor}=="0525", ATTR{idProduct}=="a700", MODE="0666"' \
  | sudo tee /etc/udev/rules.d/99-thermal-printer.rules
sudo udevadm control --reload-rules && sudo udevadm trigger

# 5. Start
docker compose up -d

# 6. Open the admin UI
open http://localhost:8000/admin
# or via Tailscale: http://<tailscale-ip>:8000/admin
```

</details>

---

## Configuration

All settings can be changed at runtime via the admin UI (`/admin → Settings`) or `PATCH /settings` — no restart needed. Env vars set the initial defaults on first run.

### Required

| Variable | Description |
|---|---|
| `ADMIN_KEY` | Secret key for all admin endpoints and the UI |

### Email notifications

| Variable | Description |
|---|---|
| `SMTP_USER` | Gmail address to send from |
| `SMTP_PASS` | [Gmail app password](https://myaccount.google.com/apppasswords) |
| `NOTIFY_EMAIL` | Recipient (defaults to `SMTP_USER`) |

### Printer

| Variable | Default | Description |
|---|---|---|
| `PRINTER_MODE` | `auto` | `auto` (Ethernet→USB), `usb`, or `network` |
| `PRINTER_HOST` | _(empty)_ | Printer IP for Ethernet mode, e.g. `192.168.1.100` |
| `PRINTER_PORT` | `9100` | ESC/POS TCP port |
| `PRINTER_USB_VENDOR` | `0x0525` | USB vendor ID from `lsusb` |
| `PRINTER_USB_PRODUCT` | `0xa700` | USB product ID from `lsusb` |
| `PRINTER_WIDTH_PX` | `384` | Printable width in pixels (58 mm ≈ 384 px, 80 mm ≈ 576 px) |

### Rate limits & CORS (runtime-editable)

| Variable | Default | Description |
|---|---|---|
| `PRINTS_PER_HOUR` | `10` | Global hourly print cap |
| `MAX_PER_IP_HOUR` | `3` | Per-IP hourly submission cap |
| `LINE_WIDTH` | `32` | Characters per line (32 for 58 mm, 48 for 80 mm) |
| `ALLOWED_ORIGIN` | `https://example.com` | CORS allowed origin for `/contact` |
| `EMAIL_NOTIFICATIONS` | `true` | Toggle email on/off |

### OpenTelemetry (optional)

| Variable | Description |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP gRPC endpoint, e.g. `http://otel-collector:4317` |
| `OTEL_SERVICE_NAME` | Service name tag in APM (default: `printer-server`) |

---

## Connecting your contact form

Add a `POST /contact` call to your existing contact form. The server accepts JSON with up to 150 characters in the message field:

```json
{
  "name":         "Ada Lovelace",
  "email":        "ada@example.com",
  "message":      "Hello from your website!",
  "browser_time": "Mon May 19  3:45 PM"
}
```

`browser_time` is optional — if omitted the server timestamps the message itself. Set `ALLOWED_ORIGIN` to your site's domain so the browser CORS preflight passes.

---

## API reference

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/contact` | public | Submit a contact form message |
| `GET` | `/health` | public | Health check + DB status + queue depth |
| `GET` | `/metrics` | public* | Prometheus metrics |
| `GET` | `/admin` | public* | Admin web UI |
| `GET` | `/queue` | admin | Queue stats |
| `POST` | `/drain` | admin | Re-queue all pending messages for printing |
| `GET` | `/messages` | admin | Paginated message history |
| `GET` | `/settings` | admin | Current runtime settings |
| `PATCH` | `/settings` | admin | Update runtime settings |
| `GET` | `/docs` | public | Interactive API docs (Swagger UI) |

*Public on Tailscale-only networks; add a reverse proxy auth layer if exposed to the internet.

**Admin auth:** pass `X-Admin-Key: <your key>` header, or enter it in the admin UI (stored in session storage — clears when the tab closes).

---

## Monitoring

### Prometheus scrape config

Add to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: pi-printer-server
    static_configs:
      - targets: ['<pi-tailscale-ip>:8000']   # app metrics

  - job_name: pi-node
    static_configs:
      - targets: ['<pi-tailscale-ip>:9100']   # system metrics (node_exporter)

  - job_name: pi-otel-collector
    static_configs:
      - targets: ['<pi-tailscale-ip>:8888']   # OTEL collector self-metrics
```

### Adding an APM backend (Datadog, Grafana Cloud, Jaeger…)

1. Edit `otel-collector.yml` — uncomment the exporter block for your backend
2. Add any required API keys to `.env`
3. Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317` in `.env`
4. `docker compose restart`

---

## Running on boot (systemd)

```bash
sudo cp printer.service /etc/systemd/system/pos-ticker.service

# Update the path if you cloned somewhere other than /opt/pos-ticker
sudo sed -i "s|/opt/pos-ticker|$(pwd)|" /etc/systemd/system/pos-ticker.service

sudo systemctl daemon-reload
sudo systemctl enable --now pos-ticker.service
sudo systemctl status pos-ticker.service
```

---

## Updating

```bash
docker compose pull
docker compose up -d
```

---

## Building from source

```bash
docker build -t pos-ticker .

# Multi-arch (requires buildx)
docker buildx build \
  --platform linux/arm64,linux/arm/v7,linux/amd64 \
  -t pos-ticker .
```

The GitHub Actions workflow (`.github/workflows/docker-publish.yml`) builds and pushes to `ghcr.io` automatically on every push to `main`.

---

## Project structure

```
.
├── printer_server.py      # FastAPI application
├── static/
│   └── index.html         # Admin UI (single file, no build step)
├── setup.sh               # Interactive installer
├── Dockerfile
├── docker-compose.yml
├── otel-collector.yml     # OTEL Collector config
├── prometheus.yml         # Scrape config reference
├── requirements.txt
├── .env.example           # Copy to .env and fill in your values
├── printer.service        # systemd unit for auto-start on boot
└── .github/
    └── workflows/
        └── docker-publish.yml
```

---

## License

MIT — see [LICENSE](LICENSE).
