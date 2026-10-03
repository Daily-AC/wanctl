package desktop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"wanctl/internal/protocol"
)

type receiptBackend struct {
	*fakeBackend
	target textReceiptTarget
}

func (b *receiptBackend) TypeText(_ protocol.DesktopWindow, text string, check func() error) error {
	return deliverTextWithReceipt(b.target, text, check)
}

func TestTypeCannotReportSuccessWithoutTextReceipt(t *testing.T) {
	for _, method := range []string{"WM_CHAR ignored", "ValuePattern ignored", "ValuePattern truncated", "unreadable after write"} {
		t.Run(method, func(t *testing.T) {
			value, writes := "", 0
			target := textReceiptTarget{read: func() (string, error) {
				if writes != 0 && method == "unreadable after write" {
					return "", errors.New("provider unavailable")
				}
				return value, nil
			}}
			if method == "WM_CHAR ignored" {
				target.character = func(uint16) error { writes++; return nil }
			} else {
				target.setValue = func(s string) error {
					writes++
					if method == "ValuePattern truncated" {
						value = s[:2]
					}
					return nil // API completed, but control did not accept the text
				}
			}
			b := &receiptBackend{fakeBackend: fake(), target: target}
			res := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{{Type: "type", Text: "notepad"}, {Type: "click", X: 64, Y: 80}})
			if res.Status != "partial" || res.Completed != 0 || res.FailedIndex != 0 || len(res.Actions) != 1 || res.Actions[0].Status != "partial" || res.Error != errTextUnverified.Error() || b.moves != 0 || b.held != 0 {
				t.Fatalf("unreceived text reported success or queued input ran: %+v moves=%d held=%d", res, b.moves, b.held)
			}
			wantWrites := 1
			if method == "WM_CHAR ignored" {
				wantWrites = len("notepad")
			}
			if writes != wantWrites {
				t.Fatalf("uncertain text replayed: %d writes, want %d", writes, wantWrites)
			}
			t.Logf("status=%s action=%s completed=%d writes=%d queued_clicks=%d", res.Status, res.Actions[0].Status, res.Completed, writes, b.moves)
		})
	}
}

func TestTextReceiptPreservesSelectionAndUnicode(t *testing.T) {
	for _, mode := range []string{"value", "characters"} {
		t.Run(mode, func(t *testing.T) {
			value := "left OLD right"
			var units []uint16
			caret := ""
			target := textReceiptTarget{before: value, prefix: "left ", suffix: " right", read: func() (string, error) { return value, nil }, caret: func(left string) error { caret = left; return nil }}
			if mode == "value" {
				target.setValue = func(s string) error { value = s; return nil }
			} else {
				target.character = func(unit uint16) error {
					units = append(units, unit)
					value = "left " + string(utf16.Decode(units)) + " right"
					return nil
				}
			}
			err := deliverTextWithReceipt(target, "中文😀", func() error { return nil })
			if err != nil || value != "left 中文😀 right" || (mode == "value" && caret != "left 中文😀") {
				t.Fatalf("insertion/selection: value=%q caret=%q err=%v", value, caret, err)
			}
		})
	}
}

func TestTextReceiptUnsupportedOrChangedTargetSendsNothing(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		writes := 0
		target := textReceiptTarget{before: "original", read: func() (string, error) { return "changed", nil }, setValue: func(string) error { writes++; return nil }}
		if unreadable {
			target.read = nil
		}
		if err := deliverTextWithReceipt(target, "x", func() error { return nil }); err == nil || writes != 0 {
			t.Fatalf("unsafe preflight: unreadable=%v writes=%d err=%v", unreadable, writes, err)
		}
	}
}

func TestTextReceiptBindsWriteAndReadToOriginalControl(t *testing.T) {
	original, stealer := "", ""
	switched := false
	target := textReceiptTarget{read: func() (string, error) { return original, nil }, setValue: func(s string) error {
		// Focus moves at the delivery boundary, after the last check. Both the
		// setter and its receipt still reference the captured original element.
		switched = true
		original = s
		return nil
	}}
	err := deliverTextWithReceipt(target, "notepad", func() error {
		if switched {
			return errors.New("foreground window changed; stopped before sending input")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "foreground window changed") || original != "notepad" || stealer != "" {
		t.Fatalf("delivery followed focus: original=%q stealer=%q err=%v", original, stealer, err)
	}
}

func TestTextReceiptHumanStopAndUncertainWriteNeverReplay(t *testing.T) {
	for _, human := range []bool{false, true} {
		sig := NewSignal()
		writes := 0
		uncertain := errors.New("provider timed out")
		target := textReceiptTarget{read: func() (string, error) { return "", nil }, setValue: func(string) error {
			writes++
			if human {
				sig.HumanInput(false)
				return nil
			}
			return uncertain
		}}
		err := deliverTextWithReceipt(target, "notepad", func() error { return sig.Check(context.Background()) })
		want := uncertain
		if human {
			want = ErrHumanInput
		}
		if !errors.Is(err, want) || writes != 1 {
			t.Fatalf("human=%v writes=%d err=%v", human, writes, err)
		}
	}
}

func TestPhysicalInputDuringVerifiedText(t *testing.T) {
	sig := NewSignal()
	sent := 0
	var at time.Time
	b := &receiptBackend{fakeBackend: fake(), target: textReceiptTarget{
		read: func() (string, error) { return "", nil },
		character: func(uint16) error {
			sent++
			if sent == 273 {
				at = time.Now()
				sig.HumanInput(false)
			}
			return nil
		},
	}}
	res := (Engine{Backend: b, Signal: sig}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{{Type: "type", Text: strings.Repeat("x", 8000)}, {Type: "click", X: 64, Y: 80}})
	if sent != 273 || at.IsZero() || time.Since(at) > 200*time.Millisecond || res.Status != "interrupted" || res.Error != protocol.DesktopHumanInput || res.Completed != 0 || res.FailedIndex != 0 || len(res.Actions) != 1 || b.moves != 0 || b.held != 0 {
		t.Fatalf("verified text did not stop cleanly: sent=%d result=%+v", sent, res)
	}
}

func TestTextReceiptWaitsForProviderWithoutResending(t *testing.T) {
	reads, writes := 0, 0
	target := textReceiptTarget{read: func() (string, error) {
		reads++
		if reads > 2 {
			return "notepad", nil
		}
		return "", nil
	}, setValue: func(string) error { writes++; return nil }}
	if err := deliverTextWithReceipt(target, "notepad", func() error { return nil }); err != nil || reads != 3 || writes != 1 {
		t.Fatalf("provider receipt: reads=%d writes=%d err=%v", reads, writes, err)
	}
}
