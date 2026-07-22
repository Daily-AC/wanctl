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
//	curl -fsSL https://<relay>/install.sh | WANCTL_TOKEN=<token> sh         # unix
//	irm https://<relay>/install.ps1 | iex                                   # windows
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
	mux.HandleFunc("/install.ps1", r.handleInstallPS1)
	mux.HandleFunc("/skills", r.handleSkills)
}

// handleSkills serves the canonical wanctl SKILL markdown. Users tell their AI
// "安装 https://***REMOVED-IP***/skills"; the AI WebFetches this URL
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

// handleInstallPS1 is the Windows counterpart to handleInstall. Windows has no
// bash by default; users (or their AI) pipe the script through PowerShell:
//
//	irm https://<relay>/install.ps1 | iex
func (r *Relay) handleInstallPS1(w http.ResponseWriter, req *http.Request) {
	scheme := "https"
	if xf := req.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	}
	base := scheme + "://" + req.Host
	portalFP := os.Getenv("WANCTL_PORTAL_FP")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, installScriptPS1, base, portalFP)
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
  set -- agent --relay "$RELAY" --token "$WANCTL_TOKEN" --transport ws --name "$NAME"
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

// installScriptPS1 is the Windows / PowerShell counterpart to installScript.
// %[1]s is the relay base URL, %[2]s the portal public-key fingerprint (may be
// empty). Same env-var contract as the sh installer: WANCTL_TOKEN (automation),
// WANCTL_NAME, WANCTL_MODE, WANCTL_BIN (install path override),
// WANCTL_INSTALL_ONLY=1 (download only, don't start the agent).
//
// When piped to 'iex', the script runs in-memory and is not subject to the
// file-based ExecutionPolicy, so it works on stock Win10/11 PowerShell.
const installScriptPS1 = `# wanctl agent installer (Windows / PowerShell).  Usage:
#   irm %[1]s/install.ps1 | iex
# Optional env: $env:WANCTL_TOKEN, $env:WANCTL_NAME, $env:WANCTL_MODE,
#   $env:WANCTL_BIN (install path; default: existing wanctl.exe in PATH, else
#   %%LOCALAPPDATA%%\wanctl\wanctl.exe), $env:WANCTL_INSTALL_ONLY=1 (don't run).
$ErrorActionPreference = 'Stop'
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch {}

$Relay    = '%[1]s'
$PortalPK = '%[2]s'

# Only amd64 binaries are published; Windows-on-ARM runs them via the kernel's
# x86_64 emulation layer just fine, so we don't need a separate arm64 build.
$bin = "wanctl-windows-amd64.exe"

$tmp = Join-Path $env:TEMP ("wanctl-" + [guid]::NewGuid().Guid + ".exe")
Write-Host "downloading $bin from $Relay/dl ..."
Invoke-WebRequest -UseBasicParsing -Uri "$Relay/dl/$bin" -OutFile $tmp

# Decide install destination. Reinstalls land at the existing binary's path so
# 'wanctl' (bare) keeps pointing at the upgraded version. First-time installs
# default to %%LOCALAPPDATA%%\wanctl\wanctl.exe — no admin needed.
$existing = (Get-Command wanctl.exe -ErrorAction SilentlyContinue).Source
if ($env:WANCTL_BIN)      { $dest = $env:WANCTL_BIN }
elseif ($existing)        { $dest = $existing }
else                      { $dest = Join-Path $env:LOCALAPPDATA 'wanctl\wanctl.exe' }
$destDir = Split-Path -Parent $dest
if (-not (Test-Path $destDir)) { New-Item -ItemType Directory -Force -Path $destDir | Out-Null }

# A running .exe can't be overwritten on Windows. Rename the old one aside
# first; the kernel keeps the open handle valid against the renamed path.
if (Test-Path $dest) {
  $old = "$dest.old"
  if (Test-Path $old) { try { Remove-Item -Force $old } catch {} }
  try { Rename-Item -Force -Path $dest -NewName ([IO.Path]::GetFileName($old)) } catch {}
}
Move-Item -Force -Path $tmp -Destination $dest
$dest = (Resolve-Path $dest).Path
$destDirAbs = (Resolve-Path (Split-Path -Parent $dest)).Path
Write-Host "installed: $dest"

# Clean up duplicate wanctl.exe elsewhere in PATH so bare 'wanctl' always
# resolves to the freshly-installed copy.
foreach ($p in ($env:PATH -split ';')) {
  if (-not $p) { continue }
  $cand = Join-Path $p 'wanctl.exe'
  if (-not (Test-Path $cand)) { continue }
  try { $candAbs = (Resolve-Path $cand).Path } catch { continue }
  if ($candAbs -ieq $dest) { continue }
  try { Remove-Item -Force $cand; Write-Host "清理旧版: $candAbs" }
  catch { Write-Host "提示: 旧版仍在 $candAbs, 请手动删除否则 'wanctl' 可能跑到旧的" }
}

# Make sure $destDir is in the user's persistent PATH (new shells) AND the
# current session's PATH (this shell). On Windows the User PATH is the right
# scope — no admin needed.
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$userParts = @()
if ($userPath) { $userParts = @($userPath -split ';' | Where-Object { $_ }) }
$onUserPath = $false
foreach ($p in $userParts) {
  try { if ((Resolve-Path $p -ErrorAction Stop).Path -ieq $destDirAbs) { $onUserPath = $true; break } } catch {}
}
if (-not $onUserPath) {
  [Environment]::SetEnvironmentVariable('Path', (($userParts + $destDirAbs) -join ';'), 'User')
  Write-Host ""
  Write-Host "已把 $destDirAbs 加进当前用户 PATH (新开 PowerShell/cmd 才生效)。"
}
$onSessionPath = $false
foreach ($p in ($env:PATH -split ';')) {
  if (-not $p) { continue }
  try { if ((Resolve-Path $p -ErrorAction Stop).Path -ieq $destDirAbs) { $onSessionPath = $true; break } } catch {}
}
if (-not $onSessionPath) { $env:PATH = "$destDirAbs;$env:PATH" }

if ($PortalPK) { $env:WANCTL_PORTAL_PK = $PortalPK }

if ($env:WANCTL_INSTALL_ONLY -eq '1') {
  Write-Host "done. run '$dest' to authorize this device (Feishu login)."
  return
}

# Automation path (e.g. an AI controller's own device): a token in the env means
# enroll non-interactively and run the agent now, like the sh installer.
if ($env:WANCTL_TOKEN) {
  $name = if ($env:WANCTL_NAME) { $env:WANCTL_NAME } else { $env:COMPUTERNAME }
  $agentArgs = @('agent','--relay',$Relay,'--token',$env:WANCTL_TOKEN,'--transport','ws','--name',$name)
  if ($env:WANCTL_MODE) { $agentArgs += @('--mode', $env:WANCTL_MODE) }
  Write-Host "starting agent as '$name' (Ctrl-C to stop; use a service wrapper to persist)"
  & $dest @agentArgs
  return
}

# Human path: no token needed. Just run 'wanctl' to log in via the browser.
Write-Host ""
Write-Host "已安装: $dest"
Write-Host "下一步: 运行下面这条完成飞书授权并把本机变成可远程控制的设备 ——"
Write-Host ""
Write-Host "    wanctl"
Write-Host ""
Write-Host "(授权后服务转入后台; 停止用 'wanctl stop')"
`
