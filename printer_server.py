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
import datetime, logging, os, queue, re, sqlite3, smtplib, ssl, threading, unicodedata
from email.mime.text import MIMEText
from pathlib import Path
from zoneinfo import ZoneInfo

from fastapi import FastAPI, Depends, HTTPException, Request
from fastapi.responses import HTMLResponse, JSONResponse, Response
from fastapi.security import APIKeyHeader
from pydantic import BaseModel
from escpos.printer import Usb, Network
from PIL import Image, ImageFont
from pilmoji import Pilmoji
from prometheus_fastapi_instrumentator import Instrumentator
from prometheus_client import Counter, Gauge

logger = logging.getLogger("uvicorn.error")

# ── Static (env-only) config ──────────────────────────────────────────────────

PRINTER_VENDOR_ID  = int(os.environ.get("PRINTER_USB_VENDOR",  "0x0525"), 16)
PRINTER_PRODUCT_ID = int(os.environ.get("PRINTER_USB_PRODUCT", "0xa700"), 16)
PRINTER_WIDTH_PX   = int(os.environ.get("PRINTER_WIDTH_PX",    "384"))

ADMIN_KEY    = os.environ.get("ADMIN_KEY", "")
SMTP_USER    = os.environ.get("SMTP_USER", "")
SMTP_PASS    = os.environ.get("SMTP_PASS", "")
NOTIFY_EMAIL = os.environ.get("NOTIFY_EMAIL", SMTP_USER)
DB_PATH      = os.environ.get("DB_PATH", "/app/data/messages.db")
# Only trust CF-Connecting-IP / X-Forwarded-For when running behind a known proxy.
TRUST_PROXY  = os.environ.get("TRUST_PROXY", "false").lower() == "true"
PACIFIC      = ZoneInfo("America/Los_Angeles")
STATIC_DIR   = Path(__file__).parent / "static"

_MONO_FONTS = [
    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
    "/usr/share/fonts/truetype/liberation/LiberationMono-Regular.ttf",
    "/usr/share/fonts/truetype/freefont/FreeMono.ttf",
]

# ── Runtime settings (stored in SQLite, editable via PATCH /settings) ────────
# Env vars provide the initial defaults on first run.

_DEFAULTS: dict[str, str] = {
    "prints_per_hour":     os.environ.get("PRINTS_PER_HOUR", "10"),
    "max_per_ip_hour":     os.environ.get("MAX_PER_IP_HOUR", "3"),
    "line_width":          os.environ.get("LINE_WIDTH", "32"),
    "allowed_origin":      os.environ.get("ALLOWED_ORIGIN", "https://your-domain.com"),
    "printer_mode":        os.environ.get("PRINTER_MODE", "auto"),  # auto|usb|network
    "printer_host":        os.environ.get("PRINTER_HOST", ""),
    "printer_port":        os.environ.get("PRINTER_PORT", "9100"),
    "email_notifications": os.environ.get("EMAIL_NOTIFICATIONS", "true"),
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


# ── Prometheus metrics ────────────────────────────────────────────────────────

_msg_counter   = Counter("printer_messages_total", "Contact form submissions", ["result"])
_print_counter = Counter("printer_prints_total",   "Print attempts",           ["result"])
_queue_gauge   = Gauge(  "printer_queue_depth",    "Messages in print queue")
_printer_up    = Gauge(  "printer_connected",      "1 = last print succeeded")

# ── OTEL traces (no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set) ─────────────

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
    _reload_cfg()
    threading.Thread(target=_printer_worker, daemon=True, name="printer-worker").start()
    if not ADMIN_KEY:
        logger.warning("ADMIN_KEY not set — admin endpoints will return 500")
    yield
    _print_queue.put(None)  # stop worker


app = FastAPI(
    title="Printer Server",
    version="3.0",
    description="Contact form → thermal printer | Admin UI at /admin",
    lifespan=lifespan,
)

Instrumentator(excluded_handlers=["/metrics", "/health"]).instrument(app).expose(app)
_setup_otel(app)


# ── CORS middleware ────────────────────────────────────────────────────────────
# Reads allowed_origin from runtime settings so it can be changed without restart.

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
        "script-src 'self' https://cdn.tailwindcss.com 'unsafe-inline'; "
        "style-src 'self' 'unsafe-inline'"
    )
    return response


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


def _prints_this_hour(c) -> int:
    return c.execute(
        "SELECT COUNT(*) FROM messages WHERE printed_at >= datetime('now','-1 hour')"
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

    if mode in ("network", "auto") and host:
        try:
            p = Network(host, port=port)
            _printer_mode_label = f"network:{host}:{port}"
            return p
        except Exception as exc:
            logger.warning("Network printer %s:%d unreachable: %s", host, port, exc)
            if mode == "network":
                raise

    p = Usb(PRINTER_VENDOR_ID, PRINTER_PRODUCT_ID, profile="default")
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


def _render_message_image(text: str, font_size: int = 22) -> Image.Image:
    font        = _load_font(font_size)
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
    # Emoji colors (yellows, oranges) map to light grey in L-mode; threshold at 200
    # so they register as black ink rather than disappearing into white.
    return img.convert("L").point(lambda x: 0 if x < 200 else 255).convert("1")


# ── UTC → Pacific ─────────────────────────────────────────────────────────────

def _utc_to_pacific(utc_str: str) -> str:
    try:
        dt = datetime.datetime.strptime(utc_str, "%Y-%m-%d %H:%M:%S")
        dt = dt.replace(tzinfo=datetime.timezone.utc).astimezone(PACIFIC)
        return dt.strftime("%a  %b %-d  %-I:%M %p")
    except Exception:
        return utc_str


def _do_print(row) -> None:
    lw  = _int_setting("line_width", 32)
    div = "─" * lw
    p   = _get_printer()
    try:
        ts      = row["browser_time"] or _utc_to_pacific(row["received_at"])
        name    = row["name"]
        email   = row["email"]
        message = row["message"]

        p.set(align="center", bold=True)
        p.text("\n" + ts + "\n")
        p.set(bold=False, align="left")
        p.text(div + "\n")

        p.text(_truncate_to_width(name, lw) + "\n")
        p.text(_truncate_to_width(email, lw) + "\n")
        p.text(div + "\n")

        p.image(_render_message_image(message))

        p.text(div + "\n")
        p.set(align="center", font="b")
        p.text(f"#{row['id']}\n")
        p.set(font="a", align="left")
        p.ln(1)
        p.cut()
    finally:
        try:
            p.close()
        except Exception:
            pass


# ── Background print queue ────────────────────────────────────────────────────

_print_queue: queue.Queue = queue.Queue()


def _printer_worker():
    while True:
        item = _print_queue.get()
        if item is None:
            break
        msg_id, row = item
        try:
            _do_print(row)
            with get_db() as c:
                c.execute(
                    "UPDATE messages SET printed_at=?, claimed=0 WHERE id=?",
                    (datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S"), msg_id),
                )
            _print_counter.labels(result="success").inc()
            _printer_up.set(1)
        except Exception as exc:
            logger.error("printer worker id=%d: %s", msg_id, exc)
            with get_db() as c:
                c.execute("UPDATE messages SET claimed=0 WHERE id=?", (msg_id,))
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
        _print_queue.put((row["id"], row))
    return {"queued_to_print": len(pending)}


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
    _print_queue.put((msg_id, row))
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
            "SELECT id, name, email, message, ip, received_at, browser_time, printed_at, claimed"
            " FROM messages ORDER BY received_at DESC LIMIT ? OFFSET ?",
            (limit, offset),
        ).fetchall()
        total = c.execute("SELECT COUNT(*) FROM messages").fetchone()[0]
    return {"total": total, "messages": [dict(r) for r in rows]}


@app.get("/settings", dependencies=[Depends(require_admin)])
async def get_settings():
    with _cfg_lock:
        return dict(_cfg)


class SettingsPatch(BaseModel):
    settings: dict[str, str]


_INT_SETTINGS = {"prints_per_hour", "max_per_ip_hour", "line_width", "printer_port"}


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
    now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S")
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


@app.post("/contact")
async def contact(request: Request):
    try:
        data = await request.json()
    except Exception:
        data = {}

    name         = (data.get("name",         "") or "").strip()[:64]
    email        = (data.get("email",        "") or "").strip()[:64]
    message      = (data.get("message",      "") or "").strip()[:500]
    browser_time = re.sub(r'[\x00-\x1f\x7f-\x9f]', '', (data.get("browser_time", "") or "").strip())[:64]

    if not (name and email and message):
        return JSONResponse({"error": "missing fields"}, status_code=400)
    if not re.match(r'^[^@\s]+@[^@\s]+\.[^@\s]+$', email):
        return JSONResponse({"error": "invalid email"}, status_code=400)

    if TRUST_PROXY:
        ip = (
            request.headers.get("CF-Connecting-IP")
            or request.headers.get("X-Forwarded-For", "").split(",")[0].strip()
            or (request.client.host if request.client else "unknown")
        )
    else:
        ip = request.client.host if request.client else "unknown"

    ua        = request.headers.get("User-Agent", "unknown")[:120]
    timestamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S")
    max_ip    = _int_setting("max_per_ip_hour", 3)
    limit     = _int_setting("prints_per_hour", 10)

    with get_db() as c:
        if _ip_submissions_this_hour(c, ip) >= max_ip:
            _msg_counter.labels(result="rate_limited_ip").inc()
            return JSONResponse({"error": "too many requests"}, status_code=429)

        cursor = c.execute(
            "INSERT INTO messages (name,email,message,ip,user_agent,received_at,browser_time)"
            " VALUES (?,?,?,?,?,?,?)",
            (name, email, message, ip, ua, timestamp, browser_time or None),
        )
        msg_id     = cursor.lastrowid
        hour_count = _prints_this_hour(c)

    _send_email(name, email, message, ip, browser_time or _utc_to_pacific(timestamp))

    if hour_count >= limit:
        logger.info("hourly limit hit (%d), message %d queued", hour_count, msg_id)
        _msg_counter.labels(result="rate_limited_global").inc()
        return JSONResponse({"ok": True, "queued": True})

    with get_db() as c:
        c.execute("UPDATE messages SET claimed=1 WHERE id=?", (msg_id,))
        row = c.execute("SELECT * FROM messages WHERE id=?", (msg_id,)).fetchone()

    _msg_counter.labels(result="accepted").inc()
    _queue_gauge.inc()
    _print_queue.put((msg_id, row))
    return JSONResponse({"ok": True, "queued": False})
