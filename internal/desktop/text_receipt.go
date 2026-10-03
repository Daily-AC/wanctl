package desktop

import (
	"errors"
	"strings"
	"time"
	"unicode/utf16"
)

var errTextUnsupported = errors.New("focused control does not support safe, verifiable text delivery; no input sent")
var errTextUnverified = errors.New("text delivery could not be confirmed in the original focused control; input may be partial; do not replay")

// All callbacks are bound to the SAME control, including readback. A successful
// window procedure/COM call alone is not a receipt. Never retry a write or switch
// transports after a write whose outcome is unknown.
type textReceiptTarget struct {
	before, prefix, suffix string
	read                   func() (string, error)
	setValue               func(string) error
	character              func(uint16) error
	caret                  func(string) error
}

func textNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func deliverTextWithReceipt(t textReceiptTarget, text string, check func() error) error {
	if t.read == nil || (t.setValue == nil && t.character == nil) {
		return errTextUnsupported
	}
	if err := check(); err != nil {
		return err
	}
	before, err := t.read()
	if err != nil || textNewlines(before) != t.before {
		return errors.New("focused text changed or cannot be read; no input sent")
	}
	left := t.prefix + textNewlines(text)
	want := left + t.suffix
	if t.setValue != nil {
		if err = check(); err != nil {
			return err
		}
		err = t.setValue(want)
	} else {
		for _, r := range text {
			if err = check(); err != nil {
				return err
			}
			for _, unit := range utf16.Encode([]rune{r}) {
				if err = t.character(unit); err != nil {
					return err
				}
			}
		}
	}
	if err != nil {
		return err
	}
	// Accessibility providers may publish an edit asynchronously. Only reads
	// are polled, with cancellation checkpoints; the input is never replayed.
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		if err = check(); err != nil {
			return err
		}
		got, readErr := t.read()
		if err = check(); err != nil {
			return err
		}
		if readErr != nil {
			return errTextUnverified
		}
		if textNewlines(got) == want {
			if t.setValue != nil && t.caret != nil {
				if err = t.caret(left); err != nil {
					if stopped := check(); stopped != nil {
						return stopped
					}
					return errors.New("text reached the original control but its insertion point could not be restored; do not replay")
				}
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errTextUnverified
		}
		time.Sleep(5 * time.Millisecond)
	}
}
