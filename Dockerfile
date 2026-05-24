# ── Stage 1: compile wheels ───────────────────────────────────────────────────
# gcc + dev headers needed to build pillow, httptools, uvloop C extensions.
# Nothing from this stage ends up in the final image.
FROM python:3.12-slim AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc \
        libjpeg-dev \
        zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
COPY requirements.txt .
RUN pip install --no-cache-dir --prefix=/install -r requirements.txt


# ── Stage 2: runtime image ────────────────────────────────────────────────────
FROM python:3.12-slim

# libjpeg62-turbo  — Pillow JPEG support at runtime (libjpeg-dev was build-only)
# libusb-1.0-0     — python-escpos USB access
# fonts-dejavu-core — monospace font for image rendering
# curl             — HEALTHCHECK
RUN apt-get update && apt-get install -y --no-install-recommends \
        libjpeg62-turbo \
        libusb-1.0-0 \
        fonts-dejavu-core \
        curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r appuser && useradd -r -g appuser appuser

WORKDIR /app

COPY --from=builder /install /usr/local

COPY --chown=appuser:appuser printer_server.py .
COPY --chown=appuser:appuser static/ static/
COPY --chown=appuser:appuser merchant-copy.ttf .

ENV DB_PATH=/app/data/messages.db
RUN mkdir -p /app/data && chown appuser:appuser /app/data

USER appuser

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -f http://localhost:8000/health || exit 1

CMD ["uvicorn", "printer_server:app", \
     "--host", "0.0.0.0", \
     "--port", "8000", \
     "--workers", "1", \
     "--log-level", "info", \
     "--access-log"]
