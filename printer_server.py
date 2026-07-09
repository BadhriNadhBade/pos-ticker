"""
Contact form → thermal printer server (v3)

FastAPI + uvicorn | Prometheus /metrics | OTEL-ready traces
Printer: Ethernet-first, USB fallback (auto-detected)
Admin UI: /admin  |  API docs: /docs

Required env:
  ADMIN_KEY     — protects all /admin/* and management endpoints
  SMTP_USER     — Gmail address
  SMTP_PASS     — Gmail app password
  NOTIFY_EMAIL  — recipient (defaults to SMTP_USER)

Optional env (override runtime-editable defaults):
  PRINTER_MODE  — auto|usb|network  (default: auto)
  PRINTER_HOST  — printer IP for network mode
  PRINTER_PORT  — ESC/POS port     (default: 9100)
  PRINTS_PER_HOUR, MAX_PER_IP_HOUR, LINE_WIDTH, ALLOWED_ORIGIN
  OTEL_EXPORTER_OTLP_ENDPOINT — enable OTLP traces (e.g. for Datadog/Jaeger)
"""

from contextlib import asynccontextmanager, contextmanager
import base64, datetime, io, json, logging, os, queue, re, sqlite3, smtplib, ssl, threading, time, unicodedata
from dataclasses import dataclass
from email.mime.text import MIMEText
from pathlib import Path
from typing import Optional
from zoneinfo import ZoneInfo

from fastapi import FastAPI, Depends, HTTPException, Request
from fastapi.responses import HTMLResponse, JSONResponse, Response
from fastapi.security import APIKeyHeader
from pydantic import BaseModel
from escpos.printer import Usb, Network
from PIL import Image, ImageFont, ImageEnhance
from pilmoji import Pilmoji
from prometheus_fastapi_instrumentator import Instrumentator
from prometheus_client import Counter, Gauge

logger = logging.getLogger("uvicorn.error")

# ── Static (env-only) config ──────────────────────────────────────────────────

PRINTER_VENDOR_ID  = int(os.environ.get("PRINTER_USB_VENDOR",  "0x0525"), 16)
PRINTER_PRODUCT_ID = int(os.environ.get("PRINTER_USB_PRODUCT", "0xa700"), 16)
PRINTER_WIDTH_PX   = int(os.environ.get("PRINTER_WIDTH_PX",    "576"))   # 80mm (3⅛") head = 576 dots; use 384 for 58mm

# ── Image input limits (untrusted bytes) ────────────────────────────────────
MAX_IMAGE_BYTES  = int(os.environ.get("MAX_IMAGE_BYTES",  str(10 * 1024 * 1024)))   # 10 MiB upload cap
MAX_IMAGE_HEIGHT = int(os.environ.get("MAX_IMAGE_HEIGHT", "1200"))                 # printed-px cap = paper guard
# Font B glyph cell ≈ 9 dots wide; leave a small margin so a full row never
# overruns the head and wraps onto a second line (which doubles the print length).
ASCII_COLUMNS    = int(os.environ.get("ASCII_COLUMNS", str(max(16, (PRINTER_WIDTH_PX - 12) // 9))))  # ≈62 @ 576px, ≈41 @ 384px
# Line feed (in dots) used for ASCII art so the vertical pitch matches the ~2:1
# cell aspect the renderer assumes; lower = tighter/shorter print.
ASCII_LINE_SPACING = int(os.environ.get("ASCII_LINE_SPACING", "18"))
# Refuse to *decode* anything beyond this many pixels (decompression-bomb guard).
Image.MAX_IMAGE_PIXELS = int(os.environ.get("MAX_IMAGE_PIXELS", str(24_000_000)))

# Raster pacing: network ESC/POS printers have a tiny input buffer and no flow
# control, so a single tall image overruns it (truncated print + dropped cut).
# Send it in short horizontal bands and pause between them so the printer drains.
IMAGE_BAND_PX    = int(os.environ.get("IMAGE_BAND_PX",    "128"))   # rows per raster chunk
IMAGE_BAND_PAUSE = float(os.environ.get("IMAGE_BAND_PAUSE", "0.2"))  # seconds between chunks

# Raster command encoding. python-escpos defaults to "bitImageRaster" (GS v 0),
# which many cheap network heads don't implement — they print the command bytes
# as literal garbage instead. "bitImageColumn" (ESC *) is the most widely
# supported; "graphics" (GS ( L) is the modern one. Override per printer.
IMAGE_IMPL = os.environ.get("IMAGE_IMPL", "bitImageColumn")

ADMIN_KEY    = os.environ.get("ADMIN_KEY", "")
SMTP_USER    = os.environ.get("SMTP_USER", "")
SMTP_PASS    = os.environ.get("SMTP_PASS", "")
NOTIFY_EMAIL = os.environ.get("NOTIFY_EMAIL", SMTP_USER)
DB_PATH      = os.environ.get("DB_PATH", "/app/data/messages.db")
TRUST_PROXY  = os.environ.get("TRUST_PROXY", "false").lower() == "true"
PACIFIC      = ZoneInfo("America/Los_Angeles")
STATIC_DIR   = Path(__file__).parent / "static"

_MONO_FONTS = [
    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
    "/usr/share/fonts/truetype/liberation/LiberationMono-Regular.ttf",
    "/usr/share/fonts/truetype/freefont/FreeMono.ttf",
]

# ── Runtime settings (stored in SQLite, editable via PATCH /settings) ────────

_DEFAULTS: dict[str, str] = {
    "prints_per_hour":        os.environ.get("PRINTS_PER_HOUR",        "10"),
    "max_per_ip_hour":        os.environ.get("MAX_PER_IP_HOUR",        "3"),
    "line_width":             os.environ.get("LINE_WIDTH",             "32"),
    "allowed_origin":         os.environ.get("ALLOWED_ORIGIN",         "https://your-domain.com"),
    "printer_mode":           os.environ.get("PRINTER_MODE",           "auto"),
    "printer_host":           os.environ.get("PRINTER_HOST",           ""),
    "printer_port":           os.environ.get("PRINTER_PORT",           "9100"),
    "email_notifications":    os.environ.get("EMAIL_NOTIFICATIONS",    "true"),
    "receipt_font":           os.environ.get("RECEIPT_FONT",           "receipt"),
    "receipt_font_size":      os.environ.get("RECEIPT_FONT_SIZE",      "22"),
    "receipt_show_timestamp": os.environ.get("RECEIPT_SHOW_TIMESTAMP", "true"),
    "receipt_show_email":     os.environ.get("RECEIPT_SHOW_EMAIL",     "true"),
    "receipt_show_id":        os.environ.get("RECEIPT_SHOW_ID",        "true"),
    "receipt_title":          os.environ.get("RECEIPT_TITLE",          ""),
    "receipt_footer":         os.environ.get("RECEIPT_FOOTER",         ""),
}

_cfg: dict[str, str] = {}
_cfg_lock = threading.Lock()


def setting(key: str) -> str:
    with _cfg_lock:
        return _cfg.get(key, _DEFAULTS.get(key, ""))


def _int_setting(key: str, default: int) -> int:
    try:
        return int(setting(key) or str(default))
    except ValueError:
        logger.warning("invalid integer setting %s=%r, using %d", key, setting(key), default)
        return default


def _reload_cfg():
    with get_db() as c:
        rows = c.execute("SELECT key, value FROM settings").fetchall()
    merged = dict(_DEFAULTS)
    merged.update({r["key"]: r["value"] for r in rows})
    with _cfg_lock:
        _cfg.clear()
        _cfg.update(merged)


def _utc_now_string() -> str:
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S")


# ── Prometheus metrics ────────────────────────────────────────────────────────

_msg_counter   = Counter("printer_messages_total", "Contact form submissions", ["result"])
_print_counter = Counter("printer_prints_total",   "Print attempts",           ["result"])
_queue_gauge   = Gauge(  "printer_queue_depth",    "Messages in print queue")
_printer_up    = Gauge(  "printer_connected",      "1 = last print succeeded")

# ── OTEL traces ───────────────────────────────────────────────────────────────

def _setup_otel(app):
    endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
    if not endpoint:
        return
    try:
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
        from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor

        provider = TracerProvider()
        provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=endpoint)))
        trace.set_tracer_provider(provider)
        FastAPIInstrumentor.instrument_app(app)
        logger.info("OTEL traces → %s", endpoint)
    except ImportError:
        logger.warning(
            "OTEL_EXPORTER_OTLP_ENDPOINT set but opentelemetry packages missing; "
            "install opentelemetry-exporter-otlp-proto-grpc"
        )


# ── App lifespan ──────────────────────────────────────────────────────────────

@asynccontextmanager
async def lifespan(app: FastAPI):
    _init_db()
    _release_stale_print_claims()
    _reload_cfg()
    threading.Thread(target=_printer_worker, daemon=True, name="printer-worker").start()
    if not ADMIN_KEY:
        logger.warning("ADMIN_KEY not set — admin endpoints will return 500")
    yield
    _print_queue.put(None)


app = FastAPI(
    title="Printer Server",
    version="3.0",
    description="Contact form → thermal printer | Admin UI at /admin",
    lifespan=lifespan,
)

Instrumentator(excluded_handlers=["/metrics", "/health"]).instrument(app).expose(app)
_setup_otel(app)


# ── CORS middleware ───────────────────────────────────────────────────────────

@app.middleware("http")
async def cors(request: Request, call_next):
    if request.method == "OPTIONS":
        response = Response(status_code=204)
    else:
        response = await call_next(request)
    origin = setting("allowed_origin")
    response.headers["Access-Control-Allow-Origin"]  = origin
    response.headers["Access-Control-Allow-Methods"] = "POST, GET, OPTIONS, PATCH"
    response.headers["Access-Control-Allow-Headers"] = "Content-Type, X-Admin-Key"
    response.headers["X-Content-Type-Options"]       = "nosniff"
    response.headers["X-Frame-Options"]              = "DENY"
    response.headers["Content-Security-Policy"] = (
        "default-src 'self'; "
        "img-src 'self' blob: data:; "
        "script-src 'self' https://cdn.tailwindcss.com 'unsafe-inline'; "
        "style-src 'self' 'unsafe-inline'"
    )
    return response


# ── Public surface gate (Cloudflare tunnel) ──────────────────────────────────
# Requests arriving through the Cloudflare tunnel carry a CF-Ray header that
# Cloudflare always injects on its edge. Public visitors cannot forge it from
# outside the tunnel. Direct LAN / Tailscale / localhost traffic never has it.
#
# We use that header as the trust boundary: through the tunnel, only the two
# contact-form endpoints exist; everything else (admin UI, /docs, /openapi.json,
# /metrics, /queue, /messages, /settings, /print, /skip, /drain, /health) is
# reachable only on the private network.

_PUBLIC_PATHS = frozenset({"/contact", "/contact/count"})


@app.middleware("http")
async def public_surface_gate(request: Request, call_next):
    if request.headers.get("CF-Ray") and request.url.path not in _PUBLIC_PATHS:
        return JSONResponse({"detail": "Not Found"}, status_code=404)
    return await call_next(request)


# ── Database ──────────────────────────────────────────────────────────────────

@contextmanager
def get_db():
    c = sqlite3.connect(DB_PATH)
    c.row_factory = sqlite3.Row
    c.execute("PRAGMA journal_mode=WAL")
    try:
        yield c
        c.commit()
    except Exception:
        c.rollback()
        raise
    finally:
        c.close()


def _init_db():
    Path(DB_PATH).parent.mkdir(parents=True, exist_ok=True)
    with get_db() as c:
        c.execute("""
            CREATE TABLE IF NOT EXISTS messages (
                id           INTEGER PRIMARY KEY AUTOINCREMENT,
                name         TEXT NOT NULL,
                email        TEXT NOT NULL,
                message      TEXT NOT NULL,
                ip           TEXT,
                user_agent   TEXT,
                received_at  TEXT NOT NULL,
                browser_time TEXT,
                printed_at   TEXT,
                claimed      INTEGER NOT NULL DEFAULT 0
            )
        """)
        c.execute("""
            CREATE TABLE IF NOT EXISTS settings (
                key        TEXT PRIMARY KEY,
                value      TEXT NOT NULL,
                updated_at TEXT NOT NULL
            )
        """)
        try:
            c.execute("ALTER TABLE messages ADD COLUMN browser_time TEXT")
        except sqlite3.OperationalError:
            pass
        try:
            c.execute("ALTER TABLE messages ADD COLUMN headers TEXT")
        except sqlite3.OperationalError:
            pass
        try:
            c.execute("ALTER TABLE messages ADD COLUMN image BLOB")
        except sqlite3.OperationalError:
            pass


def _release_stale_print_claims() -> None:
    with get_db() as c:
        released = c.execute(
            "UPDATE messages SET claimed=0 WHERE printed_at IS NULL AND claimed=1"
        ).rowcount
    if released:
        logger.warning("released %d stale in-flight print claim(s)", released)


def _prints_this_hour(c) -> int:
    return c.execute(
        "SELECT COUNT(*) FROM messages WHERE printed_at >= datetime('now','-1 hour')"
    ).fetchone()[0]


def _print_slots_used_this_hour(c) -> int:
    return c.execute(
        """
        SELECT COUNT(*) FROM messages
        WHERE printed_at >= datetime('now','-1 hour')
           OR (printed_at IS NULL AND claimed=1)
        """
    ).fetchone()[0]


def _ip_submissions_this_hour(c, ip: str) -> int:
    return c.execute(
        "SELECT COUNT(*) FROM messages WHERE ip=? AND received_at >= datetime('now','-1 hour')",
        (ip,)
    ).fetchone()[0]


# ── Auth ──────────────────────────────────────────────────────────────────────

_admin_header = APIKeyHeader(name="X-Admin-Key", auto_error=False)


async def require_admin(request: Request, key: str = Depends(_admin_header)):
    if not ADMIN_KEY:
        raise HTTPException(500, "ADMIN_KEY not configured")
    if key != ADMIN_KEY:
        ip = (
            request.headers.get("X-Forwarded-For", "").split(",")[0].strip()
            or (request.client.host if request.client else "unknown")
        )
        logger.warning("failed admin auth from %s for %s", ip, request.url.path)
        raise HTTPException(401, "unauthorized")


# ── Printer ───────────────────────────────────────────────────────────────────

_printer_mode_label = "unknown"


def _get_printer():
    global _printer_mode_label
    mode = setting("printer_mode")
    host = setting("printer_host")
    port = _int_setting("printer_port", 9100)

    if mode == "network" and not host:
        raise RuntimeError("printer_mode is 'network' but printer_host is not set")

    if mode in ("network", "auto") and host:
        try:
            p = Network(host, port=port)
            # escpos>=3.0 connects lazily — open() here so an unreachable
            # printer raises now and we can fall back / surface the error.
            p.open()
            _printer_mode_label = f"network:{host}:{port}"
            return p
        except Exception as exc:
            logger.warning("Network printer %s:%d unreachable: %s", host, port, exc)
            if mode == "network":
                raise

    p = Usb(PRINTER_VENDOR_ID, PRINTER_PRODUCT_ID, profile="default")
    p.open()
    _printer_mode_label = f"usb:{PRINTER_VENDOR_ID:04x}:{PRINTER_PRODUCT_ID:04x}"
    return p


def _load_font(size: int = 18):
    for path in _MONO_FONTS:
        try:
            return ImageFont.truetype(path, size)
        except Exception:
            continue
    logger.warning("no monospace fonts found, falling back to default font")
    return ImageFont.load_default()


def _is_ascii(text: str) -> bool:
    try:
        text.encode("ascii")
        return True
    except UnicodeEncodeError:
        return False


def _visual_len(text: str) -> int:
    return sum(2 if unicodedata.east_asian_width(ch) in ("W", "F") else 1 for ch in text)


def _truncate_to_width(text: str, max_width: int) -> str:
    w = 0
    for i, ch in enumerate(text):
        cw = 2 if unicodedata.east_asian_width(ch) in ("W", "F") else 1
        if w + cw > max_width:
            return text[:i]
        w += cw
    return text


def _wrap_text(text: str, width: int) -> list[str]:
    lines: list[str] = []
    for paragraph in (text or "").split("\n"):
        if not paragraph.strip():
            lines.append("")
            continue
        words, line = paragraph.split(), ""
        for word in words:
            candidate = (line + " " + word).strip()
            if _visual_len(candidate) > width:
                if line:
                    lines.append(line)
                line = word
            else:
                line = candidate
        if line:
            lines.append(line)
    return lines or [""]


def _render_message_image(text: str, font_size: int = 22, font_path: str = None) -> Image.Image:
    try:
        font = ImageFont.truetype(font_path, font_size) if font_path else _load_font(font_size)
    except Exception:
        font = _load_font(font_size)
    line_height = font_size + 8
    cpl         = max(20, int(PRINTER_WIDTH_PX / (font_size * 0.6)))
    lines: list[str] = []
    for paragraph in (text or "").split("\n"):
        words, line = paragraph.split(), ""
        for word in words:
            candidate = (line + " " + word).strip()
            if _visual_len(candidate) > cpl:
                if line:
                    lines.append(line)
                line = word
            else:
                line = candidate
        lines.append(line)
    height = max(line_height, len(lines) * line_height)
    img = Image.new("RGB", (PRINTER_WIDTH_PX, height), "white")
    with Pilmoji(img) as pj:
        y = 0
        for ln in lines:
            pj.text((0, y), ln, fill="black", font=font)
            y += line_height
    return img.convert("L").point(lambda x: 0 if x < 200 else 255).convert("1")


def _prepare_image(raw: bytes) -> "Image.Image":
    """Decode untrusted bytes → 1-bit raster sized for the printer.
    Raises ValueError on anything malformed or oversized."""
    if not raw:
        raise ValueError("empty image")
    if len(raw) > MAX_IMAGE_BYTES:
        raise ValueError(f"image too large (max {MAX_IMAGE_BYTES} bytes)")

    try:
        Image.open(io.BytesIO(raw)).verify()   # cheap structural check
        img = Image.open(io.BytesIO(raw))       # verify() invalidates the handle; reopen
        img.load()
    except Image.DecompressionBombError:
        raise ValueError("image dimensions too large")
    except Exception:
        raise ValueError("unreadable image")

    # Flatten transparency onto white so alpha doesn't render as solid black.
    if img.mode in ("RGBA", "LA", "P"):
        img = img.convert("RGBA")
        bg = Image.new("RGBA", img.size, (255, 255, 255, 255))
        img = Image.alpha_composite(bg, img).convert("RGB")
    else:
        img = img.convert("RGB")

    img = ImageEnhance.Contrast(img).enhance(1.4)   # thermal flattens midtones

    if img.width != PRINTER_WIDTH_PX:
        new_h = max(1, round(img.height * PRINTER_WIDTH_PX / img.width))
        img = img.resize((PRINTER_WIDTH_PX, new_h), Image.Resampling.LANCZOS)
    if img.height > MAX_IMAGE_HEIGHT:               # crop = hard paper guard
        img = img.crop((0, 0, PRINTER_WIDTH_PX, MAX_IMAGE_HEIGHT))

    return img.convert("L").convert("1")            # Floyd–Steinberg → ESC/POS raster


_ASCII_RAMP = "@%#*+=-:. "   # dark → light

def _image_to_ascii(img: "Image.Image", columns: int) -> str:
    g = img.convert("L")
    rows = max(1, round(columns * g.height / g.width * 0.5))   # chars are ~2× tall
    g = g.resize((columns, rows))
    px, ramp, n = g.load(), _ASCII_RAMP, len(_ASCII_RAMP) - 1
    return "\n".join(
        "".join(ramp[px[x, y] * n // 255] for x in range(columns)).rstrip()
        for y in range(rows)
    )


# ── Receipt fonts ─────────────────────────────────────────────────────────────

_RECEIPT_FONTS = {
    # Merchant Copy — the classic thermal "store receipt" typeface (bundled with the app).
    "receipt": str(Path(__file__).parent / "merchant-copy.ttf"),
    "mono":    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
    "sans":    "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
    "serif":   "/usr/share/fonts/truetype/dejavu/DejaVuSerif.ttf",
}


# ── UTC → Pacific ─────────────────────────────────────────────────────────────

def _utc_to_pacific(utc_str: str) -> str:
    try:
        dt = datetime.datetime.strptime(utc_str, "%Y-%m-%d %H:%M:%S")
        dt = dt.replace(tzinfo=datetime.timezone.utc).astimezone(PACIFIC)
        return dt.strftime("%a  %b %-d  %-I:%M %p")
    except Exception:
        return utc_str


# ── ESC/POS print (minimal layout) ───────────────────────────────────────────
#
#  Uses the same visual hierarchy as the preview renderer.
#  ESC/POS double-strike  = thick rule equivalent.
#  Regular dashes (─)     = thin rule.

def _do_print(row) -> None:
    lw = _int_setting("line_width", 32)

    # ── Resolve settings ──────────────────────────────────────────────────────
    title_text  = (setting("receipt_title").strip()  or "NEW MESSAGE").upper()
    footer_text = setting("receipt_footer").strip().upper()
    show_ts     = setting("receipt_show_timestamp").lower() != "false"
    show_email  = setting("receipt_show_email").lower()     != "false"
    show_id     = setting("receipt_show_id").lower()        != "false"

    ts      = row["browser_time"] or _utc_to_pacific(row["received_at"])
    name    = row["name"]
    email   = row["email"]
    msg_id  = row["id"]
    message = row["message"]

    # ── Timestamp: "Mon  May 12  2:41 PM" → split into date · time ───────────
    ts_parts = ts.rsplit("  ", 1)
    ts_date  = ts_parts[0].strip()
    ts_time  = ts_parts[1].strip() if len(ts_parts) > 1 else ""
    ts_line  = (ts_date + "  .  " + ts_time) if ts_time else ts_date

    # ── Helpers ───────────────────────────────────────────────────────────────
    thin  = "-" * lw          # thin rule   ────

    def _kv(label: str, value: str) -> str:
        """Fixed 6-char label column, value right-fills remaining width."""
        col = 6
        label_out = label[:col].upper().ljust(col)
        max_val   = lw - col - 2
        value_out = _truncate_to_width(value, max_val)
        gap       = max(1, lw - col - len(value_out))
        return label_out + " " * gap + value_out + "\n"

    p = _get_printer()
    try:
        # ── Title (centred, bold) ─────────────────────────────────────────────
        p.set(align="center", bold=True)
        p.text(_truncate_to_width(title_text, lw) + "\n")
        p.set(bold=False)

        # ── Timestamp subtitle ────────────────────────────────────────────────
        if show_ts:
            p.set(align="center")
            p.text(_truncate_to_width(ts_line, lw) + "\n")
        p.set(align="left")
        p.ln(1)

        # ── Thin rule ─────────────────────────────────────────────────────────
        p.text(thin + "\n")

        # ── Key/value rows ────────────────────────────────────────────────────
        p.text(_kv("FROM",  name))
        if show_email:
            p.text(_kv("EMAIL", email))

        # ── Message block (bold, same weight as header) ───────────────────────
        p.text(thin + "\n")
        p.set(bold=True)
        for line in _wrap_text(message, lw):
            p.text(line + "\n")
        p.set(bold=False)

        # ── Attached image (optional, full-width raster) ──────────────────────
        # Stored bytes are the ORIGINAL upload; convert to a 1-bit raster here.
        img_blob = row["image"] if "image" in row.keys() else None
        if img_blob:
            try:
                raster = _prepare_image(img_blob)
            except ValueError as exc:
                logger.warning("stored image for #%s unprintable: %s", row["id"], exc)
                raster = None
            if raster is not None:
                p.ln(1)
                p.set(align="center")
                _print_image_banded(p, raster)
                p.set(align="left")

        # ── Meta (id centred) ─────────────────────────────────────────────────
        p.text(thin + "\n")
        if show_id:
            p.set(align="center")
            p.text("#" + str(msg_id) + "\n")
            p.set(align="left")

        # ── Footer (only if configured) ───────────────────────────────────────
        if footer_text:
            p.ln(1)
            p.set(align="center")
            p.text(_truncate_to_width(footer_text, lw) + "\n")
            p.set(align="left")
        p.ln(1)
        p.cut()

    finally:
        try:
            p.close()
        except Exception:
            pass


def _print_image_banded(p, img: "Image.Image") -> None:
    """Send a raster to the printer.

    USB/local printers have no flow-control problem, so the whole image goes in a
    single command — this is gapless. Column-mode (ESC *) emits a trailing line
    feed per p.image() call, so splitting into bands would leave an extra blank
    line at every boundary; we only pay that cost for network printers, whose
    tiny input buffer overruns (truncated image + dropped cut) on a single tall
    raster and must be fed in short paced bands instead."""
    w, h = img.size
    if not _printer_mode_label.startswith("network"):
        p.image(img, impl=IMAGE_IMPL)
        return
    # Network path: snap the band to a multiple of 24 (ESC * high-density stripe
    # height) so bands tile without padding gaps, and pause after EVERY band —
    # including the last — so whatever follows (more receipt text, the cut) isn't
    # pushed into a still-draining buffer and dropped.
    band = max(24, (IMAGE_BAND_PX // 24) * 24)
    for top in range(0, h, band):
        p.image(img.crop((0, top, w, min(top + band, h))), impl=IMAGE_IMPL)
        time.sleep(IMAGE_BAND_PAUSE)


def _do_print_image(img: "Image.Image") -> None:
    p = _get_printer()
    try:
        p.set(align="center")
        _print_image_banded(p, img)   # already drains after the last band
        p.set(align="left")
        p.ln(1)
        p.cut()
    finally:
        try:
            p.close()
        except Exception:
            pass


def _do_print_ascii(text: str) -> None:
    p = _get_printer()
    try:
        p.set(align="left", font="b")   # Font B is denser; fits ~42 cols
        # Tighten the line feed so the printed pitch matches the ~2:1 cell aspect
        # the ASCII renderer assumes (Font B ≈ 9 dots wide → ~18-dot pitch). The
        # printer's ~30-dot default would otherwise stretch the image vertically.
        p.line_spacing(ASCII_LINE_SPACING)
        for line in text.split("\n"):
            p.text(line + "\n")
        p.line_spacing()               # restore printer default
        p.set(font="a")
        p.ln(1)
        p.cut()
    finally:
        try:
            p.close()
        except Exception:
            pass


# ── Background print queue ────────────────────────────────────────────────────

@dataclass
class PrintJob:
    kind: str                              # "message" | "image" | "ascii"
    msg_id: Optional[int] = None           # message jobs only
    row: Optional[sqlite3.Row] = None      # message jobs only
    image: Optional["Image.Image"] = None  # image jobs
    text: Optional[str] = None             # ascii jobs


_print_queue: queue.Queue = queue.Queue()


def _printer_worker():
    while True:
        job = _print_queue.get()
        if job is None:
            break
        try:
            if job.kind == "message":
                _do_print(job.row)
                with get_db() as c:
                    c.execute(
                        "UPDATE messages SET printed_at=?, claimed=0 WHERE id=?",
                        (_utc_now_string(), job.msg_id),
                    )
            elif job.kind == "image":
                _do_print_image(job.image)
            elif job.kind == "ascii":
                _do_print_ascii(job.text)
            _print_counter.labels(result="success").inc()
            _printer_up.set(1)
        except Exception as exc:
            logger.error("printer worker (%s): %s", job.kind, exc)
            if job.kind == "message" and job.msg_id is not None:
                with get_db() as c:
                    c.execute("UPDATE messages SET claimed=0 WHERE id=?", (job.msg_id,))
            _print_counter.labels(result="failed").inc()
            _printer_up.set(0)
        finally:
            _queue_gauge.dec()
            _print_queue.task_done()


# ── Email ─────────────────────────────────────────────────────────────────────

def _send_email(name: str, email: str, message: str, ip: str, timestamp: str):
    if setting("email_notifications").lower() != "true":
        return
    if not (SMTP_USER and SMTP_PASS and NOTIFY_EMAIL):
        return
    try:
        body = f"From:  {name} <{email}>\nTime:  {timestamp}\nIP:    {ip}\n\n{message}"
        msg = MIMEText(body)
        msg["Subject"] = f"New message from {name}"
        msg["From"]    = SMTP_USER
        msg["To"]      = NOTIFY_EMAIL
        ctx = ssl.create_default_context()
        with smtplib.SMTP_SSL("smtp.gmail.com", 465, context=ctx, timeout=10) as srv:
            srv.login(SMTP_USER, SMTP_PASS)
            srv.sendmail(SMTP_USER, NOTIFY_EMAIL, msg.as_string())
    except Exception as exc:
        logger.error("email notification failed: %s", exc)


# ── Routes ────────────────────────────────────────────────────────────────────

@app.get("/", include_in_schema=False)
async def root():
    return Response(status_code=302, headers={"Location": "/admin"})


@app.get("/admin", response_class=HTMLResponse, include_in_schema=False)
async def admin_ui():
    html = (STATIC_DIR / "index.html").read_text(encoding="utf-8")
    return HTMLResponse(html)


@app.get("/health")
async def health():
    try:
        with get_db() as c:
            c.execute("SELECT 1")
        db_ok = True
    except Exception as exc:
        logger.error("health check db error: %s", exc)
        db_ok = False
    return {
        "ok":          db_ok,
        "queue_depth": _print_queue.qsize(),
        "printer":     _printer_mode_label,
        "db":          "ok" if db_ok else "failed",
    }


@app.get("/queue", dependencies=[Depends(require_admin)])
async def queue_status():
    with get_db() as c:
        pending = c.execute(
            "SELECT COUNT(*) FROM messages WHERE printed_at IS NULL AND claimed=0"
        ).fetchone()[0]
        total = c.execute("SELECT COUNT(*) FROM messages").fetchone()[0]
        hour  = _prints_this_hour(c)
    return {
        "pending":           pending,
        "total":             total,
        "printed_this_hour": hour,
        "in_flight":         _print_queue.qsize(),
        "limit_per_hour":    _int_setting("prints_per_hour", 10),
    }


@app.post("/drain", dependencies=[Depends(require_admin)])
async def drain():
    with get_db() as c:
        c.execute("UPDATE messages SET claimed=1 WHERE printed_at IS NULL AND claimed=0")
        pending = c.execute(
            "SELECT * FROM messages WHERE claimed=1 AND printed_at IS NULL ORDER BY received_at ASC"
        ).fetchall()
    for row in pending:
        _queue_gauge.inc()
        _print_queue.put(PrintJob(kind="message", msg_id=row["id"], row=row))
    return {"queued_to_print": len(pending)}


class ImagePrint(BaseModel):
    image_b64: str
    mode: str = "bitmap"   # "bitmap" | "ascii"


# NOTE: the literal "/print/image" route MUST be declared before "/print/{msg_id}".
# FastAPI matches in declaration order, so a parameterized route placed first would
# capture "image" as msg_id and fail int parsing ("not a valid integer").
@app.post("/print/image", dependencies=[Depends(require_admin)])
async def print_image(body: ImagePrint):
    if body.mode not in ("bitmap", "ascii"):
        raise HTTPException(400, "mode must be 'bitmap' or 'ascii'")
    if len(body.image_b64) > MAX_IMAGE_BYTES * 2:   # base64 ≈ 1.33× raw; cheap pre-check
        raise HTTPException(400, "image too large")
    try:
        raw = base64.b64decode(body.image_b64, validate=True)
    except Exception:
        raise HTTPException(400, "invalid base64")
    try:
        img = _prepare_image(raw)
    except ValueError as exc:
        raise HTTPException(400, str(exc))

    if body.mode == "ascii":
        job = PrintJob(kind="ascii", text=_image_to_ascii(img, ASCII_COLUMNS))
    else:
        job = PrintJob(kind="image", image=img)

    _queue_gauge.inc()
    _print_queue.put(job)
    return {"queued": True, "mode": body.mode}


@app.post("/print/{msg_id}", dependencies=[Depends(require_admin)])
async def print_message(msg_id: int):
    with get_db() as c:
        row = c.execute(
            "SELECT * FROM messages WHERE id=? AND printed_at IS NULL AND claimed=0",
            (msg_id,),
        ).fetchone()
        if not row:
            raise HTTPException(404, "message not found or already printed/claimed")
        c.execute("UPDATE messages SET claimed=1 WHERE id=?", (msg_id,))
    _queue_gauge.inc()
    _print_queue.put(PrintJob(kind="message", msg_id=msg_id, row=row))
    return {"queued": True}


@app.post("/skip/{msg_id}", dependencies=[Depends(require_admin)])
async def skip_message(msg_id: int):
    with get_db() as c:
        affected = c.execute(
            "UPDATE messages SET claimed=2 WHERE id=? AND printed_at IS NULL AND claimed=0",
            (msg_id,),
        ).rowcount
    if not affected:
        raise HTTPException(404, "message not found or already printed/claimed")
    return {"skipped": True}


@app.get("/messages", dependencies=[Depends(require_admin)])
async def list_messages(limit: int = 50, offset: int = 0):
    with get_db() as c:
        rows = c.execute(
            "SELECT id, name, email, message, ip, received_at, browser_time, printed_at, headers, claimed,"
            " (image IS NOT NULL) AS has_image"
            " FROM messages ORDER BY received_at DESC LIMIT ? OFFSET ?",
            (limit, offset),
        ).fetchall()
        total = c.execute("SELECT COUNT(*) FROM messages").fetchone()[0]
    return {"total": total, "messages": [dict(r) for r in rows]}


@app.get("/messages/{msg_id}/image", dependencies=[Depends(require_admin)])
async def message_image(msg_id: int):
    """Serve the stored ORIGINAL upload (the real photo, not the 1-bit raster)."""
    with get_db() as c:
        row = c.execute("SELECT image FROM messages WHERE id=?", (msg_id,)).fetchone()
    if row is None or row["image"] is None:
        raise HTTPException(404, "no image")
    blob = row["image"]
    try:
        fmt = (Image.open(io.BytesIO(blob)).format or "PNG").lower()
    except Exception:
        fmt = "png"
    return Response(content=blob, media_type=f"image/{fmt}")


@app.get("/settings", dependencies=[Depends(require_admin)])
async def get_settings():
    with _cfg_lock:
        return dict(_cfg)


class SettingsPatch(BaseModel):
    settings: dict[str, str]


_INT_SETTINGS = {"prints_per_hour", "max_per_ip_hour", "line_width", "printer_port", "receipt_font_size"}


@app.patch("/settings", dependencies=[Depends(require_admin)])
async def patch_settings(body: SettingsPatch):
    unknown = set(body.settings) - set(_DEFAULTS)
    if unknown:
        raise HTTPException(400, f"Unknown settings: {sorted(unknown)}")
    for key, value in body.settings.items():
        if key in _INT_SETTINGS:
            try:
                int(value)
            except ValueError:
                raise HTTPException(400, f"Setting '{key}' must be an integer")
    now = _utc_now_string()
    with get_db() as c:
        for key, value in body.settings.items():
            c.execute(
                "INSERT INTO settings (key,value,updated_at) VALUES (?,?,?)"
                " ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at",
                (key, str(value), now),
            )
    _reload_cfg()
    with _cfg_lock:
        return dict(_cfg)


# ── Contact input validation ──────────────────────────────────────────────────
MAX_MESSAGE_CHARS = 500
_KB_ROWS = ("qwertyuiop", "asdfghjkl", "zxcvbnm")


def _has_letters(s: str) -> bool:
    """At least one real letter (any script) — rejects symbol/emoji/digit-only input."""
    return any(ch.isalpha() for ch in s)


def _is_keyboard_walk(word: str) -> bool:
    """Whole token is a straight keyboard run, e.g. 'asdf', 'qwerty', 'lkjh'."""
    return any(word in row or word in row[::-1] for row in _KB_ROWS)


def _looks_gibberish(s: str) -> bool:
    t = s.strip()
    if not t:
        return False
    # 5+ of the same character in a row: "aaaaa", "!!!!!", "......"
    if re.search(r"(.)\1{4,}", t):
        return True
    for raw in t.split():
        word = re.sub(r"[^a-z]", "", raw.lower())          # latin letters only
        if len(word) < 4:
            continue                                       # skip short / non-latin tokens
        if _is_keyboard_walk(word):                        # "asdf", "qwerty"
            return True
        if len(word) >= 6 and re.fullmatch(r"(.{2,4})\1+", word):  # "qweqwe", "asdfasdf"
            return True
        if len(word) >= 5 and not re.search(r"[aeiouy]", word):    # long run with no vowels
            return True
    return False


@app.get("/contact/count")
def contact_count():
    """Total messages that actually printed (printed_at set by the worker on success)."""
    with get_db() as c:
        n = c.execute(
            "SELECT COUNT(*) FROM messages WHERE printed_at IS NOT NULL"
        ).fetchone()[0]
    return {"printed": int(n)}


@app.post("/contact")
async def contact(request: Request):
    try:
        data = await request.json()
    except Exception:
        data = {}

    name         = (data.get("name",         "") or "").strip()[:64]
    email        = (data.get("email",        "") or "").strip()[:64]
    message      = (data.get("message",      "") or "").strip()[:1000]
    browser_time = re.sub(r'[\x00-\x1f\x7f-\x9f]', '', (data.get("browser_time", "") or "").strip())[:64]

    if not (name and message):
        return JSONResponse({"error": "missing fields"}, status_code=400)
    # email is optional; validate format only when provided
    if email and not re.match(r'^[^@\s]+@[^@\s]+\.[^@\s]+$', email):
        return JSONResponse({"error": "invalid email"}, status_code=400)
    if len(message) > MAX_MESSAGE_CHARS:
        return JSONResponse({"error": f"message too long (max {MAX_MESSAGE_CHARS} characters)"}, status_code=400)
    if not _has_letters(name) or _looks_gibberish(name):
        return JSONResponse({"error": "invalid name"}, status_code=400)
    # Message is intentionally unrestricted beyond the length cap so ASCII art and
    # symbol-only "doodles" are accepted (no real-words / gibberish check here).

    # Optional image attachment (file upload → base64 from the browser; no URL fetch).
    # We validate by running the full prepare pipeline (rejects malformed/oversized
    # uploads before they hit the queue) but store the ORIGINAL bytes so the admin
    # UI can view the real photo; the 1-bit raster for the receipt is regenerated
    # from the original at print time.
    image_blob = None
    image_b64 = data.get("image_b64", "") or ""
    if image_b64:
        if len(image_b64) > MAX_IMAGE_BYTES * 2:   # base64 ≈ 1.33× raw; cheap pre-check
            return JSONResponse({"error": "image too large"}, status_code=400)
        try:
            raw = base64.b64decode(image_b64, validate=True)
        except Exception:
            return JSONResponse({"error": "invalid image data"}, status_code=400)
        try:
            _prepare_image(raw)        # validate only; original is what we persist
        except ValueError as exc:
            return JSONResponse({"error": str(exc)}, status_code=400)
        image_blob = raw

    if TRUST_PROXY:
        ip = (
            request.headers.get("CF-Connecting-IP")
            or request.headers.get("X-Forwarded-For", "").split(",")[0].strip()
            or (request.client.host if request.client else "unknown")
        )
    else:
        ip = request.client.host if request.client else "unknown"

    ua        = request.headers.get("User-Agent", "unknown")[:120]
    headers   = json.dumps(dict(request.headers))
    timestamp = _utc_now_string()
    max_ip    = _int_setting("max_per_ip_hour", 3)
    limit     = _int_setting("prints_per_hour", 10)

    with get_db() as c:
        if _ip_submissions_this_hour(c, ip) >= max_ip:
            _msg_counter.labels(result="rate_limited_ip").inc()
            return JSONResponse({"error": "too many requests"}, status_code=429)

        cursor = c.execute(
            "INSERT INTO messages (name,email,message,ip,user_agent,received_at,browser_time,headers,image)"
            " VALUES (?,?,?,?,?,?,?,?,?)",
            (name, email, message, ip, ua, timestamp, browser_time or None, headers, image_blob),
        )
        msg_id     = cursor.lastrowid
        slots_used = _print_slots_used_this_hour(c)

        if slots_used >= limit:
            row = None
        else:
            c.execute("UPDATE messages SET claimed=1 WHERE id=?", (msg_id,))
            row = c.execute("SELECT * FROM messages WHERE id=?", (msg_id,)).fetchone()

    _send_email(name, email, message, ip, browser_time or _utc_to_pacific(timestamp))

    if row is None:
        logger.info("hourly limit hit (%d/%d), message %d left pending", slots_used, limit, msg_id)
        _msg_counter.labels(result="rate_limited_global").inc()
        return JSONResponse({"ok": True, "queued": True})

    _msg_counter.labels(result="accepted").inc()
    _queue_gauge.inc()
    _print_queue.put(PrintJob(kind="message", msg_id=msg_id, row=row))
    return JSONResponse({"ok": True, "queued": False})
