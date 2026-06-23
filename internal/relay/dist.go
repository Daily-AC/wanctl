package relay

import (
	"fmt"
	"net/http"
	"os"
)

// registerDist makes the relay serve a one-line installer and prebuilt agent
// binaries, so a new device can be enrolled with:
//
//	curl -fsSL https://<relay>/install.sh | WANCTL_TOKEN=<token> sh
//
// Binaries live in WANCTL_DIST_DIR (default /dist), populated by the Docker build.
func (r *Relay) registerDist(mux *http.ServeMux) {
	dir := os.Getenv("WANCTL_DIST_DIR")
	if dir == "" {
		dir = "/dist"
	}
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		mux.Handle("/dl/", http.StripPrefix("/dl/", http.FileServer(http.Dir(dir))))
	}
	mux.HandleFunc("/install.sh", r.handleInstall)
}

func (r *Relay) handleInstall(w http.ResponseWriter, req *http.Request) {
	scheme := "https"
	if xf := req.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	}
	base := scheme + "://" + req.Host
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprintf(w, installScript, base)
}

// installScript is a POSIX sh installer. %s is the relay base URL. It detects the
// OS/arch, downloads the matching binary, installs it, and (unless
// WANCTL_INSTALL_ONLY=1) runs the agent. Token comes from $WANCTL_TOKEN.
const installScript = `#!/bin/sh
# wanctl agent installer.  Usage:
#   curl -fsSL %[1]s/install.sh | WANCTL_TOKEN=<token> sh
# Optional env: WANCTL_NAME (default hostname), WANCTL_MODE (normal|bypass),
#   WANCTL_GUI_PORT, WANCTL_BIN (install path), WANCTL_INSTALL_ONLY=1 (don't run).
set -eu
RELAY="%[1]s"
if [ -z "${WANCTL_TOKEN:-}" ]; then echo "error: set WANCTL_TOKEN (get one from the portal)"; exit 1; fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; esac
BIN="wanctl-${OS}-${ARCH}"

TMP=$(mktemp)
echo "downloading ${BIN} from ${RELAY}/dl ..."
curl -fsSL "${RELAY}/dl/${BIN}" -o "$TMP"
chmod +x "$TMP"

DEST="${WANCTL_BIN:-/usr/local/bin/wanctl}"
if install -m755 "$TMP" "$DEST" 2>/dev/null; then :;
elif command -v sudo >/dev/null 2>&1 && sudo install -m755 "$TMP" "$DEST" 2>/dev/null; then :;
else
  mkdir -p "$HOME/.local/bin"
  install -m755 "$TMP" "$HOME/.local/bin/wanctl"
  DEST="$HOME/.local/bin/wanctl"
fi
rm -f "$TMP"
echo "installed: $DEST"

if [ "${WANCTL_INSTALL_ONLY:-}" = "1" ]; then
  echo "run: WANCTL_TOKEN=... $DEST agent --relay $RELAY --transport http --name $(hostname)"
  exit 0
fi

NAME="${WANCTL_NAME:-$(hostname)}"
set -- agent --relay "$RELAY" --token "$WANCTL_TOKEN" --transport http --name "$NAME"
[ -n "${WANCTL_MODE:-}" ] && set -- "$@" --mode "$WANCTL_MODE"
[ -n "${WANCTL_GUI_PORT:-}" ] && set -- "$@" --gui-port "$WANCTL_GUI_PORT"
echo "starting agent as '$NAME' (Ctrl-C to stop; wrap in systemd/nohup to persist)"
exec "$DEST" "$@"
`
