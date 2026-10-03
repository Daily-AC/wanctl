//go:build windows

package desktop

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"wanctl/internal/protocol"
)

// TypeText resolves one accessibility element before choosing a transport.
// ValuePattern reaches windowless edit controls (including XAML search boxes)
// which have no WM_CHAR handler. Text-only providers use addressed characters.
// Both paths read their receipt from that same element, never current focus.
func (b *nativeBackend) TypeText(expected protocol.DesktopWindow, text string, check func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := check(); err != nil {
		return err
	}
	hwnd, err := focusedTextTarget(expected)
	if err != nil {
		return err
	}
	u, err := newTextAutomation(check)
	if err != nil {
		return err
	}
	defer u.close()
	element, err := u.object(u.client, 8) // GetFocusedElement
	if err != nil {
		return err
	}
	defer element.release()
	if err = u.validateTextElement(element, expected); err != nil {
		return err
	}
	value, err := u.pattern(element, 10002, "{a94cd8b1-0844-4cd6-9d2d-640537ab39e9}")
	if err != nil {
		return err
	}
	defer value.release()
	pattern, err := u.pattern(element, 10014, "{32eba289-3583-42c9-9c59-3b6d9a1e9b6a}")
	if err != nil {
		return err
	}
	defer pattern.release()
	target, err := u.textTarget(value, pattern)
	if err != nil {
		return err
	}
	guard := func() error {
		if err := check(); err != nil {
			return err
		}
		focused, err := u.integer(element, 26) // CurrentHasKeyboardFocus
		if err != nil {
			return err
		}
		if focused == 0 {
			return errors.New("focused text control changed; stopped before sending input")
		}
		return nil
	}
	if value == nil {
		target.character = func(unit uint16) error {
			return addressedUnicode(expected, unit, true, b.physicalInputError, func(w protocol.DesktopWindow) (uintptr, error) {
				current, err := focusedTextTarget(w)
				if err != nil {
					return 0, err
				}
				if current != hwnd {
					return 0, errors.New("focused text control changed; stopped before sending input")
				}
				return hwnd, nil
			}, sendUnicodeMessage)
		}
	}
	return deliverTextWithReceipt(target, text, guard)
}

func (u *textAutomation) validateTextElement(element *automationObject, expected protocol.DesktopWindow) error {
	for _, property := range []struct {
		method uintptr
		want   int32
	}{{20, int32(expected.PID)}, {26, 1}, {28, 1}, {35, 0}} {
		got, err := u.integer(element, property.method)
		if err != nil {
			return err
		}
		if got != property.want {
			return errTextUnsupported
		}
	}
	// A process can own several windows. Walk from the captured element to a
	// native ancestor and prove it belongs to the expected HWND, not just PID.
	walker, err := u.object(u.client, 16) // RawViewWalker
	if err != nil {
		return err
	}
	defer walker.release()
	root, err := strconv.ParseUint(expected.ID, 16, 64)
	if err != nil {
		return errTextUnsupported
	}
	node := element
	for i := 0; i < 64; i++ {
		var hwnd uintptr
		err = u.call(node, 36, uintptr(unsafe.Pointer(&hwnd))) // CurrentNativeWindowHandle
		if err != nil {
			return err
		}
		if hwnd != 0 {
			ancestor, _, _ := user32.NewProc("GetAncestor").Call(hwnd, 2)
			var pid uint32
			user32.NewProc("GetWindowThreadProcessId").Call(hwnd, uintptr(unsafe.Pointer(&pid)))
			if ancestor == uintptr(root) && pid == expected.PID {
				return nil
			}
			return errTextUnsupported
		}
		node, err = u.object(walker, 3, node.address()) // GetParentElement
		if err != nil {
			return err
		}
		defer node.release()
	}
	return errTextUnsupported
}

func (u *textAutomation) documentText(pattern *automationObject) (string, error) {
	doc, err := u.object(pattern, 7) // DocumentRange
	if err != nil {
		return "", err
	}
	defer doc.release()
	return u.text(doc, 12, (1<<20)+1) // GetText, refuse oversized/truncated receipts
}

func (u *textAutomation) textTarget(value, pattern *automationObject) (t textReceiptTarget, err error) {
	if value != nil {
		readOnly, e := u.integer(value, 5)
		if e != nil {
			return t, e
		}
		if readOnly != 0 {
			return t, errTextUnsupported
		}
		t.read = func() (string, error) { return u.text(value, 4) }
		t.setValue = func(s string) error {
			return withAutomationString(s, func(bstr uintptr) error { return u.call(value, 3, bstr) })
		}
	} else if pattern != nil {
		t.read = func() (string, error) { return u.documentText(pattern) }
	} else {
		return t, errTextUnsupported
	}
	t.before, err = t.read()
	if err != nil {
		return t, err
	}
	if pattern == nil {
		// An empty ValuePattern has an unambiguous insertion point. Never
		// overwrite existing content when its selection cannot be determined.
		if t.before != "" {
			return t, errTextUnsupported
		}
		return t, nil
	}
	doc, err := u.object(pattern, 7)
	if err != nil {
		return t, err
	}
	defer doc.release()
	ranges, err := u.object(pattern, 5) // GetSelection
	if err != nil {
		return t, err
	}
	defer ranges.release()
	count, err := u.integer(ranges, 3)
	if err != nil {
		return t, err
	}
	if count != 1 {
		return t, errTextUnsupported
	}
	selection, err := u.object(ranges, 4, 0)
	if err != nil {
		return t, err
	}
	defer selection.release()
	part := func(endpoint, selectionEndpoint uintptr) (string, error) {
		r, err := u.object(doc, 3) // Clone
		if err != nil {
			return "", err
		}
		defer r.release()
		if err = u.call(r, 15, endpoint, selection.address(), selectionEndpoint); err != nil {
			return "", err
		}
		return u.text(r, 12, (1<<20)+1)
	}
	t.prefix, err = part(1, 0) // document start to selection start
	if err != nil {
		return t, err
	}
	t.suffix, err = part(0, 1) // selection end to document end
	if err != nil {
		return t, err
	}
	document, err := u.text(doc, 12, (1<<20)+1)
	if err != nil {
		return t, err
	}
	// Some providers include a final paragraph marker in TextPattern but not
	// ValuePattern. Remove only that proven extra marker, never user whitespace.
	if value != nil && document == t.before+"\n" {
		t.suffix = strings.TrimSuffix(t.suffix, "\n")
		document = t.before
	}
	if document != t.before || !strings.HasPrefix(t.before, t.prefix) || !strings.HasSuffix(t.before, t.suffix) || len(t.prefix)+len(t.suffix) > len(t.before) {
		return t, errTextUnsupported
	}
	if value != nil {
		t.caret = func(left string) error { return u.selectInsertionPoint(pattern, left) }
	}
	return t, nil
}

func (u *textAutomation) selectInsertionPoint(pattern *automationObject, left string) error {
	doc, err := u.object(pattern, 7)
	if err != nil {
		return err
	}
	defer doc.release()
	if left == "" {
		if err = u.call(doc, 15, 1, doc.address(), 0); err != nil {
			return err
		}
		return u.call(doc, 16)
	}
	return withAutomationString(left, func(bstr uintptr) error {
		r, err := u.object(doc, 8, bstr, 0, 0) // FindText, forward, case-sensitive
		if err != nil {
			return err
		}
		defer r.release()
		if err = u.call(r, 15, 0, r.address(), 1); err != nil {
			return err
		}
		return u.call(r, 16) // Select degenerate range: move insertion point
	})
}
