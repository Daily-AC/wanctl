package relay

import (
	_ "embed"
	"fmt"
	"net/http"
	"os"
)

// skillMD is the canonical wanctl SKILL.md, served at GET /skills. The relay is
// public (unlike the SSO-gated portal), so AI clients can WebFetch it directly.
//
//go:embed skill.md
var skillMD []byte

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
	mux.HandleFunc("/skills", r.handleSkills)
}

// handleSkills serves the canonical wanctl SKILL markdown. Users tell their AI
// "安装 https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills"; the AI WebFetches this URL
// and writes the response to ~/.claude/skills/wanctl/SKILL.md.
func (r *Relay) handleSkills(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="SKILL.md"`)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(skillMD)
}

func (r *Relay) handleInstall(w http.ResponseWriter, req *http.Request) {
	scheme := "https"
	if xf := req.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	}
	base := scheme + "://" + req.Host
	portalFP := os.Getenv("WANCTL_PORTAL_FP")
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprintf(w, installScript, base, portalFP)
}

// installScript is a POSIX sh installer. %[1]s is the relay base URL,
// %[2]s is the portal public-key fingerprint (may be empty). It detects the
// OS/arch, downloads the matching binary, installs it, and (unless
// WANCTL_INSTALL_ONLY=1) runs the agent. Token comes from $WANCTL_TOKEN.
const installScript = `#!/bin/sh
# wanctl agent installer.  Usage:
#   curl -fsSL %[1]s/install.sh | WANCTL_TOKEN=<token> sh
# Optional env: WANCTL_NAME (default hostname), WANCTL_MODE (normal|bypass),
#   WANCTL_BIN (install path), WANCTL_INSTALL_ONLY=1 (don't run).
set -eu
RELAY="%[1]s"
PORTAL_PK="%[2]s"

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

[ -n "$PORTAL_PK" ] && export WANCTL_PORTAL_PK="$PORTAL_PK"

if [ "${WANCTL_INSTALL_ONLY:-}" = "1" ]; then
  echo "done. run '$DEST' to authorize this device (Feishu login)."
  exit 0
fi

# Automation path (e.g. an AI controller's own device): a token in the env means
# enroll non-interactively and run the agent now, like before.
if [ -n "${WANCTL_TOKEN:-}" ]; then
  NAME="${WANCTL_NAME:-$(hostname)}"
  set -- agent --relay "$RELAY" --token "$WANCTL_TOKEN" --transport http --name "$NAME"
  [ -n "${WANCTL_MODE:-}" ] && set -- "$@" --mode "$WANCTL_MODE"
  echo "starting agent as '$NAME' (Ctrl-C to stop; wrap in systemd/nohup to persist)"
  exec "$DEST" "$@"
fi

# Human path: no token needed. Just run 'wanctl' to log in via the browser.
echo ""
echo "✓ 已安装: $DEST"
echo "下一步: 运行下面这条完成飞书授权并把本机变成可远程控制的设备 ——"
echo ""
echo "    $DEST"
echo ""
echo "(授权后服务转入后台; 停止用 '$DEST stop')"
`
