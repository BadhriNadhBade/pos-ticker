FROM python:3.12-slim

# libusb-1.0-0       — python-escpos USB access
# fonts-dejavu-core  — monospace font for image rendering
# curl               — HEALTHCHECK
RUN apt-get update && apt-get install -y --no-install-recommends \
        libusb-1.0-0 \
        fonts-dejavu-core \
        curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY printer_server.py .
COPY static/ static/

# SQLite DB lives on a named volume at /app/data
ENV DB_PATH=/app/data/messages.db

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -f http://localhost:8000/health || exit 1

CMD ["uvicorn", "printer_server:app", \
     "--host", "0.0.0.0", \
     "--port", "8000", \
     "--workers", "1", \
     "--log-level", "info", \
     "--access-log"]
