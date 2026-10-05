package agent

import (
	"io"
	"strings"
)

// Android shell failures sometimes arrive on stdout, sometimes stderr, and can
// cross write boundaries. The shell's combined output needs only a small tail
// to recognize the permission denial; no command content is added to the hint.
type androidPermissionWriter struct {
	out     io.Writer
	tail    string
	command string
	denied  bool
}

func (w *androidPermissionWriter) Write(p []byte) (int, error) {
	s := strings.ToLower(w.tail + string(p))
	if strings.Contains(s, "inject_events") || ((strings.Contains(s, "screencap") || strings.Contains(strings.ToLower(w.command), "screencap")) && (strings.Contains(s, "permission denied") || strings.Contains(s, "permission denial") || strings.Contains(s, "permission failure"))) {
		w.denied = true
	}
	if len(s) > 4096 {
		s = s[len(s)-4096:]
	}
	w.tail = s
	return w.out.Write(p)
}

const androidPermissionHint = "Android denied screen/input permission: use wanctl exec --elevate for input commands (enable the device elevation channel), or use wanctl screenshot / wanctl_screenshot to capture the screen"
