// Package desktop implements one visible, cancellable batch in the logged-in
// Windows user's desktop. Helpers are short-lived, including from session 0.
package desktop

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"wanctl/internal/protocol"
)

const MaxActions = 64
const MaxText = 16384

// Summary is the ONLY action representation allowed into approvals, logs and
// notifications. No arbitrary strings (including keys, titles or paths) pass
// through it, even for malformed requests.
func Summary(action string, req *protocol.DesktopRequest, coordinates bool) string {
	if action == "screenshot" {
		return "screenshot"
	}
	var parts []string
	if req != nil {
		for _, a := range req.Actions {
			t := a.Type
			switch t {
			case "click", "drag", "scroll", "type", "key", "wait", "focus", "launch":
			default:
				t = "invalid"
			}
			if coordinates {
				switch t {
				case "type":
					t += fmt.Sprintf("(%d characters)", utf8.RuneCountInString(a.Text))
				case "click", "scroll":
					t += fmt.Sprintf("(%d,%d)", a.X, a.Y)
				case "drag":
					t += fmt.Sprintf("(%d,%d to %d,%d)", a.X, a.Y, a.ToX, a.ToY)
				}
			}
			parts = append(parts, t)
		}
	}
	return "act " + strings.Join(parts, ",")
}

func Validate(action string, req *protocol.DesktopRequest) error {
	if req == nil {
		return errors.New("desktop request missing")
	}
	if action == "screenshot" {
		if len(req.Actions) != 0 {
			return errors.New("screenshot does not accept actions")
		}
		if req.Region != nil && req.ScreenshotID == "" {
			return errors.New("region needs a full screenshot_id")
		}
		return nil
	}
	if action != "act" {
		return errors.New("desktop action must be screenshot or act")
	}
	if req.Region != nil {
		return errors.New("act does not accept region")
	}
	if req.ScreenshotID == "" {
		return errors.New("act needs a fresh screenshot_id")
	}
	if len(req.Actions) == 0 || len(req.Actions) > MaxActions {
		return errors.New("act needs 1 to 64 actions")
	}
	total := 0
	for i, a := range req.Actions {
		var err error
		switch a.Type {
		case "click", "drag", "scroll":
			if a.Button != "" && a.Button != "left" && a.Button != "right" && a.Button != "middle" {
				err = errors.New("invalid mouse button")
			}
			if a.Count < 0 || a.Count > 2 {
				err = errors.New("click count must be 1 or 2")
			}
			if a.Delta < -100 || a.Delta > 100 {
				err = errors.New("scroll delta exceeds 100 notches")
			}
			if a.Millis < 0 || a.Millis > 10000 {
				err = errors.New("drag duration exceeds 10000 ms")
			}
		case "type":
			total += utf8.RuneCountInString(a.Text)
			if !utf8.ValidString(a.Text) || strings.ContainsRune(a.Text, 0) || total > MaxText {
				err = errors.New("invalid Unicode text or batch exceeds 16384 characters")
			}
		case "key":
			_, err = ParseKeys(a.Key)
		case "wait":
			if a.Millis < 0 || a.Millis > 10000 {
				err = errors.New("wait must be 0 to 10000 ms")
			}
		case "focus":
			if (a.Title == "") == (a.PID == 0) {
				err = errors.New("focus needs exactly one of title or pid")
			}
		case "launch":
			if a.Program == "" || strings.ContainsRune(a.Program, 0) {
				err = errors.New("launch needs a program")
			}
			if a.TimeoutMS < 0 || a.TimeoutMS > 30000 {
				err = errors.New("launch timeout must be 0 to 30000 ms")
			}
		default:
			err = errors.New("unknown action type")
		}
		if err != nil {
			return fmt.Errorf("action %d: %w", i, err)
		}
	}
	return nil
}

// Key names are translated to virtual keys only for MapVirtualKeyEx; injection
// uses scan codes (and the extended flag), not layout-dependent text keys.
func ParseKeys(combo string) ([]uint16, error) {
	named := map[string]uint16{"ctrl": 0x11, "alt": 0x12, "shift": 0x10, "win": 0x5b, "enter": 0x0d, "tab": 9, "esc": 0x1b, "escape": 0x1b, "space": 0x20, "backspace": 8, "delete": 0x2e, "insert": 0x2d, "home": 0x24, "end": 0x23, "pageup": 0x21, "pagedown": 0x22, "left": 0x25, "up": 0x26, "right": 0x27, "down": 0x28}
	var keys []uint16
	seen := map[uint16]bool{}
	for _, s := range strings.Split(strings.ToLower(combo), "+") {
		v, ok := named[s]
		if !ok && len(s) == 1 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= '0' && s[0] <= '9')) {
			v = uint16(strings.ToUpper(s)[0])
			ok = true
		}
		if !ok {
			for i := 1; i <= 12; i++ {
				if s == fmt.Sprintf("f%d", i) {
					v = uint16(0x6f + i)
					ok = true
				}
			}
		}
		if !ok || seen[v] || len(keys) == 8 {
			return nil, errors.New("unsupported key combination; use documented key names")
		}
		seen[v] = true
		keys = append(keys, v)
	}
	return keys, nil
}
