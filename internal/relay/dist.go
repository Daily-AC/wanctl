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
// Note: hosts that drive wanctl via the MCP server (`wanctl mcp`) do NOT need a
// skill — the tool descriptions are self-describing. This skill only exists for
// Bash-only hosts that shell out to `wanctl exec/push/pull/…`.
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
#   WANCTL_BIN (install path; default: existing 'wanctl' in PATH, else
#   /usr/local/bin/wanctl), WANCTL_INSTALL_ONLY=1 (don't run).
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

# Decide install destination. Reinstalls should land at the existing binary's
# path so 'wanctl' (bare) keeps pointing at the upgraded version. First-time
# installs default to /usr/local/bin/wanctl, falling back to ~/.local/bin if
# we can't get sudo for it.
EXISTING=$(command -v wanctl 2>/dev/null || true)
DEST="${WANCTL_BIN:-${EXISTING:-/usr/local/bin/wanctl}}"
DEST_DIR=$(dirname "$DEST")
mkdir -p "$DEST_DIR" 2>/dev/null || (command -v sudo >/dev/null 2>&1 && sudo mkdir -p "$DEST_DIR") || true

if install -m755 "$TMP" "$DEST" 2>/dev/null; then :;
elif command -v sudo >/dev/null 2>&1 && sudo install -m755 "$TMP" "$DEST" 2>/dev/null; then :;
else
  mkdir -p "$HOME/.local/bin"
  install -m755 "$TMP" "$HOME/.local/bin/wanctl"
  DEST="$HOME/.local/bin/wanctl"
fi
rm -f "$TMP"
DEST_DIR=$(dirname "$DEST")
DEST_DIR_ABS=$(cd "$DEST_DIR" && pwd -P)
DEST="$DEST_DIR_ABS/$(basename "$DEST")"
echo "installed: $DEST"

# Clean up duplicate wanctl binaries elsewhere in PATH so bare 'wanctl' always
# resolves to the freshly-installed copy. Old installs left over from an earlier
# fallback (~/.local/bin vs /usr/local/bin) are exactly the trap this prevents.
oldifs=$IFS
IFS=':'
for P in $PATH; do
  IFS=$oldifs
  [ -z "$P" ] && { IFS=':'; continue; }
  CAND="$P/wanctl"
  [ -e "$CAND" ] || { IFS=':'; continue; }
  PABS=$(cd "$P" 2>/dev/null && pwd -P) || { IFS=':'; continue; }
  if [ "$PABS/wanctl" = "$DEST" ]; then IFS=':'; continue; fi
  if rm -f "$CAND" 2>/dev/null; then
    echo "清理旧版: $CAND"
  elif command -v sudo >/dev/null 2>&1 && sudo rm -f "$CAND" 2>/dev/null; then
    echo "清理旧版 (sudo): $CAND"
  else
    echo "提示: 旧版仍在 $CAND, 请手动 rm 否则 'wanctl' 可能跑到旧的"
  fi
  IFS=':'
done
IFS=$oldifs

# PATH hint: if $DEST_DIR isn't in PATH, the user's shell won't find 'wanctl'.
case ":$PATH:" in
  *":$DEST_DIR:"*) ;;
  *)
    echo ""
    echo "提示: $DEST_DIR 不在你的 PATH 里。把这行加进 shell 配置 (~/.zshrc 或 ~/.bashrc):"
    echo "    export PATH=\"$DEST_DIR:\$PATH\""
    echo "然后 'source ~/.zshrc' 或重开终端，'wanctl' 才能直接被找到。"
    ;;
esac

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
echo "已安装: $DEST"
echo "下一步: 运行下面这条完成飞书授权并把本机变成可远程控制的设备 ——"
echo ""
echo "    wanctl"
echo ""
echo "(授权后服务转入后台; 停止用 'wanctl stop')"
`
