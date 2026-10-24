#!/usr/bin/env bash
# Interactive setup for pos-ticker (Go version).
#
# Two install methods:
#   1) Docker Compose  - pulls ghcr.io image, optional Cloudflare Tunnel / observability profiles
#   2) Single binary   - downloads a release binary, installs a hardened systemd unit
#
# USB printers use the kernel `usblp` driver (/dev/usb/lp*). This script loads the
# module, persists it, and installs a udev rule so group `lp` can open the device.
#
# Env overrides: POS_TICKER_REPO (default badhrinadhbade/pos-ticker),
#                POS_TICKER_VERSION (binary install; default latest),
#                POS_TICKER_BIN (binary install from a local file instead of downloading).
set -euo pipefail

REPO="${POS_TICKER_REPO:-badhrinadhbade/pos-ticker}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${REPO_DIR}/.env"

BIN_PATH="/usr/local/bin/pos-ticker"
SVC_USER="pos-ticker"
SVC_NAME="pos-ticker"
SYS_ENV_DIR="/etc/pos-ticker"
SYS_ENV_FILE="${SYS_ENV_DIR}/pos-ticker.env"
UDEV_RULE_FILE="/etc/udev/rules.d/99-pos-ticker-usblp.rules"
UDEV_RULE='SUBSYSTEM=="usbmisc", KERNEL=="lp[0-9]*", GROUP="lp", MODE="0660"'

# ── Terminal colours ──────────────────────────────────────────────────────────
if [ -t 1 ]; then
  RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
  BLUE='\033[0;34m'; BOLD='\033[1m'; NC='\033[0m'
else
  RED=''; GREEN=''; YELLOW=''; BLUE=''; BOLD=''; NC=''
fi

info()    { echo -e "${BLUE}->${NC} $*"; }
ok()      { echo -e "${GREEN}OK${NC} $*"; }
warn()    { echo -e "${YELLOW}!${NC}  $*"; }
die()     { echo -e "${RED}ERROR${NC} $*" >&2; exit 1; }
header()  { echo; echo -e "${BOLD}$*${NC}"; printf '%.0s-' {1..48}; echo; }

SUDO=""
if [[ $EUID -ne 0 ]]; then
  command -v sudo &>/dev/null || die "Run as root or install sudo."
  SUDO="sudo"
fi

# ── Prompt helpers ────────────────────────────────────────────────────────────

# Values end up in a file read by both docker compose and systemd, so keep them
# to a conservative character set (no quotes, $, #, backslash or spaces).
safe_value() { [[ "$1" =~ ^[A-Za-z0-9@._:/+=,-]*$ ]]; }

ask() {           # ask VAR "prompt" "default"   (free text, no validation)
  local _var="$1" _msg="$2" _def="${3:-}" _in
  if [[ -n "$_def" ]]; then
    read -rp "  $_msg [$_def]: " _in
    printf -v "$_var" '%s' "${_in:-$_def}"
  else
    read -rp "  $_msg: " _in
    printf -v "$_var" '%s' "$_in"
  fi
}

ask_safe() {      # ask_safe VAR "prompt" "default"  (re-asks until value is env-file safe)
  local _var="$1" _msg="$2" _def="${3:-}" _val
  while true; do
    ask _val "$_msg" "$_def"
    if safe_value "$_val"; then printf -v "$_var" '%s' "$_val"; return; fi
    warn "Only letters, digits and  @ . _ : / + = , -  are allowed here."
  done
}

ask_secret() {    # ask_secret VAR "prompt"
  local _var="$1" _msg="$2" _in
  while true; do
    read -rsp "  $_msg: " _in; echo
    if safe_value "$_in"; then printf -v "$_var" '%s' "$_in"; return; fi
    warn "Only letters, digits and  @ . _ : / + = , -  are allowed here."
  done
}

confirm() {       # confirm "prompt" y|n   -> 0 for yes
  local _msg="$1" _def="${2:-n}" _in _hint="[y/N]"
  [[ "$_def" == "y" ]] && _hint="[Y/n]"
  read -rp "  $_msg $_hint: " _in
  _in="${_in:-$_def}"
  [[ "${_in,,}" == "y" || "${_in,,}" == "yes" ]]
}

envval() {        # envval KEY FILE -> value of KEY (last assignment), empty if absent
  grep -E "^$1=" "$2" 2>/dev/null | tail -1 | cut -d= -f2- || true
}

gen_key() {
  if command -v openssl &>/dev/null; then
    openssl rand -hex 24
  elif command -v python3 &>/dev/null; then
    python3 -c "import secrets; print(secrets.token_hex(24))"
  else
    head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

# ── 1. Install method ─────────────────────────────────────────────────────────

header "1 / 8  Install method"
echo "    1) Docker Compose   (recommended; optional Cloudflare Tunnel / metrics stack)"
echo "    2) Single binary + systemd   (no Docker)"
ask METHOD "Choose" "1"
case "$METHOD" in
  2) METHOD="binary" ;;
  *) METHOD="docker" ;;
esac

# ── 2. Prerequisites ──────────────────────────────────────────────────────────

header "2 / 8  Checking prerequisites"

DOCKER=(docker)
if [[ "$METHOD" == "docker" ]]; then
  if ! command -v docker &>/dev/null; then
    warn "Docker not found."
    if confirm "Install Docker automatically via get.docker.com?" y; then
      command -v curl &>/dev/null || die "curl is required to install Docker."
      curl -fsSL https://get.docker.com | $SUDO sh
      if [[ -n "$SUDO" ]]; then $SUDO usermod -aG docker "$USER"; fi
      ok "Docker installed. (Log out and back in to use it without sudo.)"
    else
      die "Docker is required. Install it (https://docs.docker.com/engine/install/) then re-run setup."
    fi
  fi
  if ! docker info &>/dev/null; then
    if $SUDO docker info &>/dev/null; then
      DOCKER=($SUDO docker)
      warn "Using sudo for docker (your user is not in the docker group yet)."
    else
      die "Cannot talk to the Docker daemon. Is it running?"
    fi
  fi
  "${DOCKER[@]}" compose version &>/dev/null \
    || die "Docker Compose plugin missing. Run: sudo apt install docker-compose-plugin"
  ok "Docker $("${DOCKER[@]}" --version | awk '{print $3}' | tr -d ',')"
else
  [[ "$(uname -s)" == "Linux" ]] || die "The systemd install is Linux-only."
  command -v systemctl &>/dev/null || die "systemd (systemctl) is required for the binary install."
  if [[ -z "${POS_TICKER_BIN:-}" ]]; then
    for t in curl tar sha256sum; do
      command -v "$t" &>/dev/null || die "'$t' is required."
    done
  fi
  ok "systemd found"
fi

# ── 3. USB printer (usblp) ────────────────────────────────────────────────────

header "3 / 8  USB printer (usblp)"

shopt -s nullglob
LP_DEVS=(/dev/usb/lp*)
shopt -u nullglob

USB_FOUND=0
VENDOR_ID="0x0525"
PRODUCT_ID="0xa700"
USB_DEVICE=""

if [[ "$(uname -s)" == "Linux" ]]; then
  if [[ -d /sys/module/usblp ]]; then
    ok "usblp kernel module is available"
  else
    info "Loading usblp kernel module ..."
    if $SUDO modprobe usblp 2>/dev/null; then ok "usblp loaded"; else warn "modprobe usblp failed (module missing from this kernel?)"; fi
  fi
  # Load on every boot.
  if [[ ! -f /etc/modules-load.d/usblp.conf ]]; then
    if echo usblp | $SUDO tee /etc/modules-load.d/usblp.conf >/dev/null 2>&1; then
      ok "usblp will load at boot (/etc/modules-load.d/usblp.conf)"
    else
      warn "Could not write /etc/modules-load.d/usblp.conf"
    fi
  fi
  sleep 1
  shopt -s nullglob; LP_DEVS=(/dev/usb/lp*); shopt -u nullglob

  if [[ ${#LP_DEVS[@]} -gt 0 ]]; then
    USB_FOUND=1
    USB_DEVICE="${LP_DEVS[0]}"
    ok "Found ${LP_DEVS[*]}"
    _sys="/sys/class/usbmisc/$(basename "$USB_DEVICE")/device/.."
    if [[ -r "$_sys/idVendor" && -r "$_sys/idProduct" ]]; then
      VENDOR_ID="0x$(cat "$_sys/idVendor")"
      PRODUCT_ID="0x$(cat "$_sys/idProduct")"
      ok "Vendor/product ${VENDOR_ID}:${PRODUCT_ID}"
    fi
    if [[ ${#LP_DEVS[@]} -gt 1 ]]; then
      warn "Several lp devices found; using ${USB_DEVICE}. Set PRINTER_USB_DEVICE in the env file to change."
    fi
  else
    warn "No /dev/usb/lp* device. Fine for network printers."
    if command -v lsusb &>/dev/null; then
      _line=$(lsusb 2>/dev/null | grep -iE 'print|escpos|thermal|bixolon|epson|star |citizen|rongta|sewoo|xprinter' | head -1 || true)
      if [[ -n "$_line" ]]; then
        info "lsusb sees: $_line"
        info "If it is a USB printer, make sure it is switched on and in 'printer' mode, then re-run."
      fi
    fi
  fi

  # udev: let group `lp` (not world) open the node.
  if confirm "Install udev rule giving group 'lp' access to /dev/usb/lp*?" y; then
    getent group lp &>/dev/null || $SUDO groupadd -r lp
    if printf '%s\n' "$UDEV_RULE" | $SUDO tee "$UDEV_RULE_FILE" >/dev/null \
       && $SUDO udevadm control --reload-rules \
       && $SUDO udevadm trigger --subsystem-match=usbmisc; then
      ok "udev rule installed -> $UDEV_RULE_FILE"
    else
      warn "Could not install udev rule. Run manually:"
      info "echo '$UDEV_RULE' | sudo tee $UDEV_RULE_FILE && sudo udevadm control --reload-rules"
    fi
    if [[ -f /etc/udev/rules.d/99-thermal-printer.rules ]] \
       && confirm "Remove the old libusb udev rule (99-thermal-printer.rules, no longer needed)?" y; then
      $SUDO rm -f /etc/udev/rules.d/99-thermal-printer.rules
      $SUDO udevadm control --reload-rules || true
    fi
  fi
fi
LP_GID="$(getent group lp 2>/dev/null | cut -d: -f3 || true)"
LP_GID="${LP_GID:-7}"

# ── Existing configuration? ───────────────────────────────────────────────────

KEEP_ENV=0
if [[ "$METHOD" == "docker" && -f "$ENV_FILE" ]]; then
  header "Existing .env found"
  if confirm "Keep it and skip configuration questions?" y; then KEEP_ENV=1; fi
elif [[ "$METHOD" == "binary" && -f "$SYS_ENV_FILE" ]]; then
  header "Existing ${SYS_ENV_FILE} found"
  if confirm "Keep it and skip configuration questions?" y; then KEEP_ENV=1; fi
fi

ADMIN_KEY=""; PRINTER_MODE="auto"; ADMIN_HOST="127.0.0.1"; ADMIN_PORT=8000
TUNNEL_TOKEN=""; PROFILES=""; USB_PASSTHROUGH=0

if [[ $KEEP_ENV -eq 0 ]]; then

# ── 4. Printer settings ───────────────────────────────────────────────────────

header "4 / 8  Printer settings"

echo "  Connection mode:"
echo "    auto     - network first if an IP is set, then USB  (recommended)"
echo "    usb      - USB only"
echo "    network  - Ethernet/WiFi only"
ask_safe PRINTER_MODE "Mode" "auto"
case "$PRINTER_MODE" in auto|usb|network) ;; *) die "Mode must be auto, usb or network." ;; esac

PRINTER_HOST=""
if [[ "$PRINTER_MODE" != "usb" ]]; then
  ask_safe PRINTER_HOST "Printer IP address (blank if USB only)" ""
fi

echo
echo "  Paper width:"
echo "    1)  58 mm  ->  384 px   most desktop receipt printers"
echo "    2)  80 mm  ->  576 px   wider POS printers"
read -rp "  Choose [1/2]: " _pw
case "$_pw" in
  2) PRINTER_WIDTH_PX=576; LINE_WIDTH=48 ;;
  *) PRINTER_WIDTH_PX=384; LINE_WIDTH=32 ;;
esac
ok "${PRINTER_WIDTH_PX} px / ${LINE_WIDTH} chars per line"

# ── 5. Admin key ──────────────────────────────────────────────────────────────

header "5 / 8  Admin key"

echo "  Protects the admin UI, API and /metrics (minimum 16 characters)."
while true; do
  read -rsp "  Enter a key, or press Enter to generate a random one: " ADMIN_KEY; echo
  if [[ -z "$ADMIN_KEY" ]]; then
    ADMIN_KEY="$(gen_key)"
    ok "Generated a ${#ADMIN_KEY}-character key (shown at the end)."
    break
  fi
  if [[ ${#ADMIN_KEY} -lt 16 ]]; then warn "Too short (${#ADMIN_KEY}); need at least 16."; continue; fi
  if ! safe_value "$ADMIN_KEY"; then warn "Only letters, digits and  @ . _ : / + = , -  are allowed."; continue; fi
  break
done

# ── 6. Email ──────────────────────────────────────────────────────────────────

header "6 / 8  Email notifications (optional)"

echo "  Get an email on every new contact form submission."
echo "  Requires a Gmail address with an App Password:"
echo "  https://myaccount.google.com/apppasswords"
echo

SMTP_USER=""; SMTP_PASS=""; NOTIFY_EMAIL=""; EMAIL_NOTIFICATIONS="false"
if confirm "Set up email notifications?" n; then
  ask_safe   SMTP_USER    "Gmail address" ""
  ask_secret SMTP_PASS    "App password (no spaces)"
  ask_safe   NOTIFY_EMAIL "Send notifications to" "$SMTP_USER"
  EMAIL_NOTIFICATIONS="true"
  ok "Email notifications enabled -> $NOTIFY_EMAIL"
fi

# ── 7. Exposure, origin, timezone, profiles ──────────────────────────────────

header "7 / 8  Network exposure"

echo "  Admin UI/API/metrics must stay on loopback, LAN or Tailscale - never behind a tunnel."
_ts_ip=""
if command -v tailscale &>/dev/null; then
  _ts_ip="$(tailscale ip -4 2>/dev/null | head -1 || true)"
fi
_def_admin="127.0.0.1"
if [[ -n "$_ts_ip" ]]; then
  info "Tailscale detected: this host is ${_ts_ip}"
  if confirm "Bind the admin UI to the Tailscale address (${_ts_ip})?" y; then _def_admin="$_ts_ip"; fi
fi
ask_safe ADMIN_HOST "Admin listener address (127.0.0.1, a Tailscale/LAN IP, or 0.0.0.0)" "$_def_admin"
ask_safe ADMIN_PORT "Admin port" "8000"

echo
echo "  The domain of the site that hosts your contact form (CORS), e.g. https://yourname.com"
ask_safe ALLOWED_ORIGIN "Origin" "https://example.com"

_tz_def="America/Los_Angeles"
if [[ -r /etc/timezone ]]; then _tz_def="$(tr -d '[:space:]' < /etc/timezone)"
elif command -v timedatectl &>/dev/null; then _tz_def="$(timedatectl show -p Timezone --value 2>/dev/null || echo "$_tz_def")"; fi
ask_safe DISPLAY_TZ "Timezone for receipt timestamps" "$_tz_def"

TRUST_PROXY="false"
if [[ "$METHOD" == "docker" ]]; then
  header "Optional services (Docker profiles)"
  echo "  tunnel         Cloudflare Tunnel exposing ONLY the public contact-form listener"
  echo "  observability  Prometheus + node-exporter + OpenTelemetry collector"
  if confirm "Enable the Cloudflare Tunnel profile?" n; then
    echo "  Create a tunnel in Cloudflare Zero Trust -> Networks -> Tunnels and copy its token."
    echo "  Then map your public hostname to  http://printer:8080  (NOT port 8000)."
    ask_secret TUNNEL_TOKEN "Tunnel token"
    PROFILES="tunnel"
    TRUST_PROXY="true"
  fi
  if confirm "Enable the observability profile?" n; then
    PROFILES="${PROFILES:+$PROFILES,}observability"
  fi
else
  echo
  info "Cloudflare Tunnel (optional): install cloudflared separately and point its public"
  info "hostname at http://127.0.0.1:8080 only, then set TRUST_PROXY=true in ${SYS_ENV_FILE}."
fi

# USB passthrough for docker
if [[ "$METHOD" == "docker" && "$PRINTER_MODE" != "network" ]]; then
  if [[ $USB_FOUND -eq 1 ]]; then
    USB_PASSTHROUGH=1
  elif [[ "$PRINTER_MODE" == "usb" ]] && confirm "No lp device found now. Enable USB passthrough anyway (container will not start until the printer is present)?" n; then
    USB_PASSTHROUGH=1
    USB_DEVICE="/dev/usb/lp0"
  fi
fi

# ── 8. Write config & start ──────────────────────────────────────────────────

header "8 / 8  Writing config & starting"

_common_env() {
cat <<EOF
ADMIN_KEY=${ADMIN_KEY}

DISPLAY_TZ=${DISPLAY_TZ}
ALLOWED_ORIGIN=${ALLOWED_ORIGIN}
TRUST_PROXY=${TRUST_PROXY}

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
EOF
if [[ -n "$USB_DEVICE" ]]; then echo "PRINTER_USB_DEVICE=${USB_DEVICE}"; fi
cat <<EOF

PRINTS_PER_HOUR=10
MAX_PER_IP_HOUR=3
LINE_WIDTH=${LINE_WIDTH}
EOF
}

if [[ "$METHOD" == "docker" ]]; then
  if [[ -f "$ENV_FILE" ]]; then
    _bak="${ENV_FILE}.bak.$(date +%s)"
    cp -p "$ENV_FILE" "$_bak"; chmod 600 "$_bak"
    info "Existing .env backed up to $(basename "$_bak")"
  fi
  umask 077
  {
    _common_env
    echo
    echo "# Docker compose: host-side bind addresses for the published ports."
    echo "ADMIN_BIND=${ADMIN_HOST}"
    echo "PUBLIC_BIND=127.0.0.1"
    echo "TUNNEL_TOKEN=${TUNNEL_TOKEN}"
    echo "PRINTER_GID=${LP_GID}"
    if [[ -n "$PROFILES" ]]; then echo "COMPOSE_PROFILES=${PROFILES}"; fi
    if [[ $USB_PASSTHROUGH -eq 1 ]]; then echo "COMPOSE_FILE=docker-compose.yml:docker-compose.usb.yml"; fi
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  ok ".env written (mode 600)"
else
  _tmp_env="$(mktemp)"
  {
    _common_env
    echo
    echo "ADMIN_ADDR=${ADMIN_HOST}:${ADMIN_PORT}"
    echo "PUBLIC_ADDR=127.0.0.1:8080"
  } > "$_tmp_env"
  if [[ -f "$SYS_ENV_FILE" ]]; then $SUDO cp -p "$SYS_ENV_FILE" "${SYS_ENV_FILE}.bak.$(date +%s)"; fi
  $SUDO install -d -m 0750 "$SYS_ENV_DIR"
  $SUDO install -m 0600 -o root -g root "$_tmp_env" "$SYS_ENV_FILE"
  rm -f "$_tmp_env"
  ok "${SYS_ENV_FILE} written (mode 600, root-owned; systemd reads it)"
fi

fi  # KEEP_ENV

# ── Start: Docker ─────────────────────────────────────────────────────────────

if [[ "$METHOD" == "docker" ]]; then
  if [[ $KEEP_ENV -eq 1 ]]; then
    ADMIN_KEY="$(envval ADMIN_KEY "$ENV_FILE")"
    ADMIN_HOST="$(envval ADMIN_BIND "$ENV_FILE")"; ADMIN_HOST="${ADMIN_HOST:-127.0.0.1}"
    ADMIN_PORT=8000
    PROFILES="$(envval COMPOSE_PROFILES "$ENV_FILE")"
  fi
  cd "$REPO_DIR"
  info "Pulling image ..."
  "${DOCKER[@]}" compose pull --quiet
  info "Starting services ..."
  "${DOCKER[@]}" compose up -d
  _view_host="$ADMIN_HOST"; [[ "$_view_host" == "0.0.0.0" ]] && _view_host="localhost"
  _logs="docker compose logs -f"
fi

# ── Start: single binary ──────────────────────────────────────────────────────

if [[ "$METHOD" == "binary" ]]; then
  if [[ -n "${POS_TICKER_BIN:-}" ]]; then
    [[ -x "$POS_TICKER_BIN" ]] || die "POS_TICKER_BIN=$POS_TICKER_BIN is not an executable file."
    _bin_src="$POS_TICKER_BIN"; _unit_src="${REPO_DIR}/printer.service"
  else
    case "$(uname -m)" in
      x86_64|amd64)  _arch=amd64 ;;
      aarch64|arm64) _arch=arm64 ;;
      armv7l|armv8l) _arch=armv7 ;;
      *) die "Unsupported CPU architecture: $(uname -m) (release binaries: amd64, arm64, armv7)." ;;
    esac
    _ver="${POS_TICKER_VERSION:-latest}"
    [[ "$_ver" =~ ^[A-Za-z0-9._-]+$ ]] || die "Invalid POS_TICKER_VERSION: $_ver"
    if [[ "$_ver" == "latest" ]]; then
      _base="https://github.com/${REPO}/releases/latest/download"
    else
      _base="https://github.com/${REPO}/releases/download/${_ver}"
    fi
    _asset="pos-ticker_linux_${_arch}.tar.gz"
    _dl="$(mktemp -d)"; trap 'rm -rf "$_dl"' EXIT
    info "Downloading ${_asset} (${_ver}) ..."
    curl -fsSL -o "${_dl}/${_asset}" "${_base}/${_asset}" || die "Download failed: ${_base}/${_asset}"
    curl -fsSL -o "${_dl}/checksums.txt" "${_base}/checksums.txt" || die "Could not download checksums.txt"
    # Integrity: the tarball must match its entry in checksums.txt.
    grep -q " ${_asset}\$" "${_dl}/checksums.txt" || die "${_asset} is not listed in checksums.txt - aborting."
    ( cd "$_dl" && grep " ${_asset}\$" checksums.txt | sha256sum -c - ) || die "Checksum mismatch - aborting."
    ok "Checksum verified (integrity)"
    # Authenticity: checksums.txt is signed keyless by this repository's release
    # workflow; a checksum file fetched from the same place as the tarball proves
    # nothing on its own. Verify the signature when cosign is available.
    if command -v cosign &>/dev/null; then
      curl -fsSL -o "${_dl}/checksums.txt.sigstore.json" "${_base}/checksums.txt.sigstore.json" \
        || die "cosign is installed but the signature bundle could not be downloaded: ${_base}/checksums.txt.sigstore.json"
      _repo_re="$(printf '%s' "$REPO" | sed 's/\./\\./g')"
      # Identity = this repository's release workflow running on a v* tag.
      cosign verify-blob \
        --bundle "${_dl}/checksums.txt.sigstore.json" \
        --certificate-identity-regexp "^https://github\.com/${_repo_re}/\.github/workflows/docker-publish\.yml@refs/tags/v.+\$" \
        --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
        "${_dl}/checksums.txt" >/dev/null 2>&1 \
        || die "cosign signature verification of checksums.txt FAILED - aborting (do not install this download)."
      ok "Signature verified with cosign (authenticity: signed by github.com/${REPO} release workflow)"
    else
      warn "cosign is not installed: only INTEGRITY was checked (SHA-256 vs checksums.txt), NOT AUTHENTICITY."
      warn "Anyone able to replace the release assets could replace checksums.txt too. Install cosign"
      warn "(https://docs.sigstore.dev/cosign/installation/) and re-run to verify the publisher's signature."
    fi
    tar -xzf "${_dl}/${_asset}" -C "$_dl"
    _bin_src="${_dl}/pos-ticker"; _unit_src="${_dl}/printer.service"
  fi
  [[ -f "$_unit_src" ]] || die "systemd unit not found at $_unit_src"

  id "$SVC_USER" &>/dev/null \
    || $SUDO useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
  $SUDO install -m 0755 -o root -g root "$_bin_src" "$BIN_PATH"
  $SUDO install -m 0644 -o root -g root "$_unit_src" "/etc/systemd/system/${SVC_NAME}.service"
  ok "Installed $BIN_PATH and ${SVC_NAME}.service"

  if [[ $KEEP_ENV -eq 1 ]]; then
    _a="$(envval ADMIN_ADDR "$SYS_ENV_FILE")"; _a="${_a:-127.0.0.1:8000}"
    ADMIN_HOST="${_a%:*}"; ADMIN_PORT="${_a##*:}"
    ADMIN_KEY="$(envval ADMIN_KEY "$SYS_ENV_FILE")"
  fi
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable --now "${SVC_NAME}.service"
  sleep 2
  if $SUDO systemctl is-active --quiet "${SVC_NAME}.service"; then ok "Service is running"
  else warn "Service is not active - see: sudo journalctl -u ${SVC_NAME} -e"; fi
  _view_host="$ADMIN_HOST"; [[ "$_view_host" == "0.0.0.0" ]] && _view_host="localhost"
  _logs="sudo journalctl -u ${SVC_NAME} -f"
fi

# ── Done ──────────────────────────────────────────────────────────────────────

echo
echo -e "${GREEN}${BOLD}Setup complete!${NC}"
echo
printf '%.0s-' {1..48}; echo
printf '  %-14s %s\n' "Admin UI"  "http://${_view_host}:${ADMIN_PORT}/admin"
printf '  %-14s %s\n' "Health"    "http://${_view_host}:${ADMIN_PORT}/health"
if [[ -n "${PROFILES:-}" ]]; then
  printf '  %-14s %s\n' "Profiles" "$PROFILES"
  if [[ "$PROFILES" == *observability* ]]; then printf '  %-14s %s\n' "Prometheus" "http://127.0.0.1:9090"; fi
fi
printf '%.0s-' {1..48}; echo
echo
if [[ -n "$ADMIN_KEY" ]]; then
  echo -e "  Admin key:  ${BOLD}${ADMIN_KEY}${NC}   (also stored in the env file)"
  echo
fi
echo "  Useful commands:"
echo "    ${_logs}"
if [[ "$METHOD" == "docker" ]]; then
  echo "    docker compose restart"
  echo "    docker compose down"
  echo "    docker compose pull && docker compose up -d     # update"
  echo
  echo "  Containers restart automatically (restart: unless-stopped); no systemd unit needed."
else
  echo "    sudo systemctl restart ${SVC_NAME}"
  echo "    sudo systemctl status ${SVC_NAME}"
  echo "    sudo \$EDITOR ${SYS_ENV_FILE} && sudo systemctl restart ${SVC_NAME}"
fi
echo
