#!/usr/bin/env bash
# Interactive setup for pos-ticker
# Detects your printer, writes .env, installs the udev rule, and starts Docker.
set -euo pipefail

IMAGE="ghcr.io/badhrinadhbade/pos-ticker:latest"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Terminal colours ──────────────────────────────────────────────────────────
if [ -t 1 ]; then
  RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
  BLUE='\033[0;34m'; BOLD='\033[1m'; NC='\033[0m'
else
  RED=''; GREEN=''; YELLOW=''; BLUE=''; BOLD=''; NC=''
fi

info()    { echo -e "${BLUE}→${NC} $*"; }
ok()      { echo -e "${GREEN}✓${NC} $*"; }
warn()    { echo -e "${YELLOW}!${NC} $*"; }
die()     { echo -e "${RED}✗${NC} $*"; exit 1; }
header()  { echo; echo -e "${BOLD}$*${NC}"; printf '%.0s─' {1..48}; echo; }

ask() {           # ask VAR "prompt" "default"
  local _var="$1" _msg="$2" _def="${3:-}"
  if [[ -n "$_def" ]]; then
    read -rp "  $_msg [$_def]: " _in
    printf -v "$_var" '%s' "${_in:-$_def}"
  else
    read -rp "  $_msg: " _in
    printf -v "$_var" '%s' "$_in"
  fi
}

ask_secret() {    # ask_secret VAR "prompt"
  local _var="$1" _msg="$2"
  read -rsp "  $_msg: " _in; echo
  printf -v "$_var" '%s' "$_in"
}

# ── 1. Docker ─────────────────────────────────────────────────────────────────

header "1 / 7  Checking Docker"

if ! command -v docker &>/dev/null; then
  warn "Docker not found."
  read -rp "  Install Docker automatically? [Y/n]: " _ans
  if [[ "${_ans,,}" != "n" ]]; then
    info "Installing Docker via get.docker.com …"
    curl -fsSL https://get.docker.com | sh
    sudo usermod -aG docker "$USER"
    ok "Docker installed. Log out and back in if you get permission errors later."
  else
    die "Docker is required. Install it (https://docs.docker.com/engine/install/) then re-run setup."
  fi
else
  ok "Docker $(docker --version | awk '{print $3}' | tr -d ',')"
fi

docker compose version &>/dev/null \
  || die "Docker Compose plugin missing. Run: sudo apt install docker-compose-plugin"

# ── 2. USB printer detection ─────────────────────────────────────────────────

header "2 / 7  Detecting USB Printer"

VENDOR_ID="0x0525"
PRODUCT_ID="0xa700"

if command -v lsusb &>/dev/null; then
  _line=$(lsusb 2>/dev/null \
    | grep -iE 'print|escpos|thermal|bixolon|epson|star |citizen|rongta|sewoo|xprinter' \
    | head -1 || true)
  if [[ -n "$_line" ]]; then
    _ids=$(echo "$_line" | grep -oP 'ID \K[0-9a-f]{4}:[0-9a-f]{4}' || true)
    if [[ -n "$_ids" ]]; then
      VENDOR_ID="0x${_ids%%:*}"
      PRODUCT_ID="0x${_ids##*:}"
      ok "Detected: $_line"
    fi
  else
    warn "No USB printer found on USB bus. You can still use Ethernet mode."
    info "If your printer is USB, connect it and re-run setup, or set IDs manually below."
  fi
fi

# ── 3. Printer settings ───────────────────────────────────────────────────────

header "3 / 7  Printer Settings"

echo "  Connection mode:"
echo "    auto     — try Ethernet first, fall back to USB  (recommended)"
echo "    usb      — USB only"
echo "    network  — Ethernet/WiFi only"
ask PRINTER_MODE "Mode" "auto"

PRINTER_HOST=""
if [[ "$PRINTER_MODE" != "usb" ]]; then
  ask PRINTER_HOST "Printer IP address (leave blank if USB only)" ""
fi

echo
echo "  Paper width:"
echo "    1)  58 mm  →  384 px   most desktop receipt printers"
echo "    2)  80 mm  →  576 px   wider POS printers"
read -rp "  Choose [1/2]: " _pw
case "$_pw" in
  2) PRINTER_WIDTH_PX=576; LINE_WIDTH=48 ;;
  *) PRINTER_WIDTH_PX=384; LINE_WIDTH=32 ;;
esac
ok "${PRINTER_WIDTH_PX} px / ${LINE_WIDTH} chars per line"

# ── 4. Admin key ──────────────────────────────────────────────────────────────

header "4 / 7  Admin Key"

if command -v openssl &>/dev/null; then
  _gen=$(openssl rand -hex 24)
elif command -v python3 &>/dev/null; then
  _gen=$(python3 -c "import secrets; print(secrets.token_hex(24))")
else
  _gen="pos-ticker-$(date +%s)-$(id -u)"
fi

echo "  Protects /admin and all management endpoints."
echo "  Auto-generated: ${BOLD}${_gen}${NC}"
read -rp "  Press Enter to use it, or type your own: " _custom
ADMIN_KEY="${_custom:-$_gen}"

# ── 5. Email notifications ────────────────────────────────────────────────────

header "5 / 7  Email Notifications (optional)"

echo "  Get an email on every new contact form submission."
echo "  Requires a Gmail address with an App Password:"
echo "  https://myaccount.google.com/apppasswords"
echo
read -rp "  Set up email notifications? [y/N]: " _email_yn

SMTP_USER=""; SMTP_PASS=""; NOTIFY_EMAIL=""; EMAIL_NOTIFICATIONS="false"

if [[ "${_email_yn,,}" == "y" ]]; then
  ask        SMTP_USER   "Gmail address"
  ask_secret SMTP_PASS   "App password"
  ask        NOTIFY_EMAIL "Send notifications to" "$SMTP_USER"
  EMAIL_NOTIFICATIONS="true"
  ok "Email notifications enabled → $NOTIFY_EMAIL"
fi

# ── 6. CORS origin ────────────────────────────────────────────────────────────

header "6 / 7  Website Origin"

echo "  The domain of the site that hosts your contact form."
echo "  Example: https://yourname.com"
ask ALLOWED_ORIGIN "Origin" "https://example.com"

# ── 7. Write .env + udev + start ─────────────────────────────────────────────

header "7 / 7  Writing config & starting services"

# .env
cat > "${REPO_DIR}/.env" <<EOF
ADMIN_KEY=${ADMIN_KEY}

SMTP_USER=${SMTP_USER}
SMTP_PASS=${SMTP_PASS}
NOTIFY_EMAIL=${NOTIFY_EMAIL:-$SMTP_USER}
EMAIL_NOTIFICATIONS=${EMAIL_NOTIFICATIONS}

PRINTER_MODE=${PRINTER_MODE}
PRINTER_HOST=${PRINTER_HOST}
PRINTER_PORT=9100
PRINTER_USB_VENDOR=${VENDOR_ID}
PRINTER_USB_PRODUCT=${PRODUCT_ID}
PRINTER_WIDTH_PX=${PRINTER_WIDTH_PX}

PRINTS_PER_HOUR=10
MAX_PER_IP_HOUR=3
LINE_WIDTH=${LINE_WIDTH}
ALLOWED_ORIGIN=${ALLOWED_ORIGIN}
EOF
chmod 600 "${REPO_DIR}/.env"
ok ".env written (mode 600)"

# udev rule — only for USB mode on Linux
if [[ "$PRINTER_MODE" != "network" ]] && command -v udevadm &>/dev/null; then
  _v="${VENDOR_ID#0x}"; _p="${PRODUCT_ID#0x}"
  _rule="SUBSYSTEM==\"usb\", ATTR{idVendor}==\"${_v}\", ATTR{idProduct}==\"${_p}\", MODE=\"0666\""
  _udev="/etc/udev/rules.d/99-thermal-printer.rules"
  if sudo sh -c "echo '$_rule' > $_udev" 2>/dev/null \
     && sudo udevadm control --reload-rules \
     && sudo udevadm trigger; then
    ok "udev rule installed → $_udev"
  else
    warn "Could not write udev rule (needs sudo). USB access may fail."
    info "Run manually:  echo '$_rule' | sudo tee $_udev && sudo udevadm control --reload-rules"
  fi
fi

# Pull + start
cd "$REPO_DIR"
info "Pulling image …"
docker compose pull --quiet
info "Starting services …"
docker compose up -d

# ── Done ──────────────────────────────────────────────────────────────────────

echo
echo -e "${GREEN}${BOLD}Setup complete!${NC}"
echo
printf '%.0s─' {1..48}; echo
printf '  %-18s %s\n' "Admin UI"  "http://localhost:8000/admin"
printf '  %-18s %s\n' "API docs"  "http://localhost:8000/docs"
printf '  %-18s %s\n' "Health"    "http://localhost:8000/health"
printf '%.0s─' {1..48}; echo
echo
echo -e "  Admin key:  ${BOLD}${ADMIN_KEY}${NC}"
echo
echo "  Useful commands:"
echo "    docker compose logs -f        — live logs"
echo "    docker compose restart        — restart"
echo "    docker compose down           — stop"
echo "    docker compose pull && docker compose up -d  — update to latest image"
echo
echo "  To run on boot:  see 'Running as a system service' in README.md"
echo
