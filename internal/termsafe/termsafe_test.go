package termsafe

import (
	"bytes"
	"testing"
)

func TestEscapeMakesControlsVisibleAndKeepsText(t *testing.T) {
	cases := map[string]string{
		"remote error: \x1b[2J\x1b[Hall good": `remote error: \x1b[2J\x1b[Hall good`,
		"title\x1b]0;owned\x07":               `title\x1b]0;owned\x07`,
		"c1 \u009b31m csi":                    `c1 \u009b31m csi`,
		"raw \x9b byte":                       `raw \x9b byte`,
		"nul\x00 del\x7f bell\x07":            `nul\x00 del\x7f bell\x07`,
		"line one\nline\ttwo\r\nthree":        "line one\nline\ttwo\r\nthree",
		"\rwanctl: ok":                        `\x0dwanctl: ok`,
		"设备 已拒绝: 路径 /srv/数据 — ok ✓ \uFFFD": "设备 已拒绝: 路径 /srv/数据 — ok ✓ \uFFFD",
	}
	for in, want := range cases {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
	// Escaping what Escape produced changes nothing: a caller that escapes
	// text another layer already escaped does not mangle it.
	for in := range cases {
		once := Escape(in)
		if twice := Escape(once); twice != once {
			t.Errorf("Escape is not idempotent on %q: %q then %q", in, once, twice)
		}
	}
}

func TestWriterEscapesAStream(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out)
	chunks := [][]byte{
		[]byte("progress 10%\rprogress 90%\r\n\x1b[31mred"),
		{0xe4}, {0xbd, 0xa0}, // 你, split across writes
		[]byte("好\n"),
		{0xc2}, {0x9b}, // U+009B split across writes
		[]byte("\ttab\n"),
		{0xe5, 0xa5}, // the start of a character the stream never finishes
	}
	for _, c := range chunks {
		n, err := w.Write(c)
		if err != nil || n != len(c) {
			t.Fatalf("Write(%q) = %d, %v", c, n, err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "progress 10%\rprogress 90%\r\n\\x1b[31mred你好\n\\u009b\ttab\n\\xe5\\xa5"
	if got := out.String(); got != want {
		t.Fatalf("stream = %q\nwant     %q", got, want)
	}
}
