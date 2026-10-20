package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCondenseShowsWhatTheTerminalShowed(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"colour", "\x1b[1;32mok\x1b[0m done\n", "ok done\n"},
		{"erasing and the cursor", "\x1b[2K\x1b[1Gstep 3/3\x1b[?25h\n", "step 3/3\n"},
		{"hyperlink", "see \x1b]8;;https://example.com\x1b\\docs\x1b]8;;\x1b\\\n", "see docs\n"},
		{"window title", "\x1b]0;building\x07built\n", "built\n"},
		{"charset", "plain\x1b(B\x1b[m\n", "plain\n"},
		{"progress frames", "  0%\r 50%\r100%\ndone\n", "100%\ndone\n"},
		{"frame erased before the next", "fetching\r\x1b[Kfetched\n", "fetched\n"},
		{"frame ended by its carriage return", "frame=  30 fps=0.0\rframe=  60 fps=59\r\n", "frame=  60 fps=59\n"},
		{"frame blanked with spaces", "downloading 100%\r                \nok\n", "downloading 100%\nok\n"},
		{"CRLF is a line ending", "one\r\ntwo\r\n", "one\ntwo\n"},
	} {
		if got := condenseOutput(tc.in, 0); got != tc.want {
			t.Errorf("%s: condenseOutput(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestCondenseCollapsesRepeatedLines(t *testing.T) {
	in := strings.Repeat("waiting for db\n", 40) + "ready\n"
	want := "waiting for db\n[... previous line repeated 39 more times ...]\nready\n"
	if got := condenseOutput(in, 0); got != want {
		t.Errorf("condenseOutput = %q, want %q", got, want)
	}
	// Up to foldAfter in a row stay, and blank lines are not folded at all.
	for _, in := range []string{"a\na\na\nb\n", "x\n\n\n\n\n\ny\n"} {
		if got := condenseOutput(in, 0); got != in {
			t.Errorf("condenseOutput(%q) = %q, want it unchanged", in, got)
		}
	}
}

// A failed go test -v: the passing tests around the failure fold away, and
// everything that says what failed stays — including the name of the test
// whose log comes before its FAIL line.
func TestCondenseFoldsThePassingTestsOfAFailedRun(t *testing.T) {
	var in strings.Builder
	for i := range 12 {
		fmt.Fprintf(&in, "=== RUN   TestOK%d\n--- PASS: TestOK%d (0.00s)\n", i, i)
	}
	in.WriteString("=== RUN   TestBroken\n    broken_test.go:9: want 2, got 3\n--- FAIL: TestBroken (0.00s)\n")
	for i := range 4 {
		fmt.Fprintf(&in, "=== RUN   TestLater%d\n    --- PASS: TestLater%d/sub (0.00s)\n", i, i)
	}
	in.WriteString("FAIL\nexit status 1\nFAIL\texample.com/m\t0.012s\n")

	want := "[... 24 passing-test lines omitted ...]\n" +
		"=== RUN   TestBroken\n    broken_test.go:9: want 2, got 3\n--- FAIL: TestBroken (0.00s)\n" +
		"[... 8 passing-test lines omitted ...]\n" +
		"FAIL\nexit status 1\nFAIL\texample.com/m\t0.012s\n"
	if got := condenseOutput(in.String(), 1); got != want {
		t.Errorf("failed run condensed to\n%s\nwant\n%s", got, want)
	}
	if got := condenseOutput(in.String(), 0); got != in.String() {
		t.Errorf("a run that passed lost its list of tests:\n%s", got)
	}
}

// Each runner's passing lines fold; its failures, skips, logs and summaries
// are what a failed run is read for, and every one of them stays.
func TestCondenseFoldsOnlyPassingTests(t *testing.T) {
	for _, tc := range []struct {
		runner string
		passed []string
		kept   []string
	}{
		{"go test ./...",
			[]string{"ok  \texample.com/m/a\t0.010s", "ok  \texample.com/m/b\t(cached)", "?   \texample.com/m/cmd\t[no test files]", "ok  \texample.com/m/c\t1.2s\tcoverage: 80.0% of statements"},
			[]string{"--- FAIL: TestX (0.00s)", "--- SKIP: TestY (0.00s)", "FAIL", "FAIL\texample.com/m/d\t0.020s"}},
		{"cargo test",
			[]string{"test a::one ... ok", "test a::two ... ok", "test b::three ... ok", "test src/lib.rs - add (line 5) ... ok"},
			[]string{"running 6 tests", "test b::four ... FAILED", "test b::five ... ignored", "test result: FAILED. 4 passed; 1 failed; 1 ignored; 0 measured; 0 filtered out; finished in 0.00s"}},
		{"pytest -v",
			[]string{"tests/test_a.py::test_one PASSED                 [ 20%]", "tests/test_a.py::test_two PASSED                 [ 40%]", "tests/test_a.py::TestK::test_three PASSED        [ 60%]", "tests/test_a.py::test_four PASSED                [ 80%]"},
			[]string{"tests/test_a.py::test_five FAILED                [100%]", "FAILED tests/test_a.py::test_five - assert 1 == 2", "========= 1 failed, 4 passed in 0.05s ========="}},
		{"jest",
			[]string{"PASS src/a.test.js", "  ✓ adds (2 ms)", "  ✓ subtracts (1 ms)", "  √ multiplies (1 ms)"},
			[]string{"FAIL src/b.test.js", "  ✕ divides (3 ms)", "Tests:       1 failed, 3 passed, 4 total"}},
		{"vitest",
			[]string{" ✓ src/a.test.ts (3 tests) 4ms", "   ✓ adds 1ms", "   ✓ subtracts 0ms", "   ✓ multiplies 0ms"},
			[]string{" ❯ src/b.test.ts (1 test | 1 failed) 6ms", "   × divides 5ms", " Test Files  1 failed | 1 passed (2)"}},
		{"bun test",
			[]string{"(pass) math > adds [0.08ms]", "(pass) math > subtracts [0.02ms]", "(pass) math > multiplies [0.01ms]", "✓ math > squares [0.01ms]"},
			[]string{"(fail) math > divides [0.30ms]", "(skip) math > later", " 4 pass", " 1 fail"}},
		{"mocha, node --test",
			[]string{"    ✔ adds", "    ✔ subtracts", "✔ multiplies (0.4ms)", "✔ squares (0.1ms)"},
			[]string{"    1) divides", "✖ divides (1.2ms)", "  4 passing (6ms)", "  1 failing"}},
	} {
		in := strings.Join(tc.passed, "\n") + "\n" + strings.Join(tc.kept, "\n") + "\n"
		want := fmt.Sprintf("[... %d passing-test lines omitted ...]\n", len(tc.passed)) + strings.Join(tc.kept, "\n") + "\n"
		if got := condenseOutput(in, 1); got != want {
			t.Errorf("%s: failed run condensed to\n%s\nwant\n%s", tc.runner, got, want)
		}
		if got := condenseOutput(in, 0); got != in {
			t.Errorf("%s: a run that passed was condensed to\n%s", tc.runner, got)
		}
	}
}

func TestCondenseKeepsShortRunsOfPassingTests(t *testing.T) {
	in := "--- FAIL: TestA (0.00s)\n--- PASS: TestB (0.00s)\n--- PASS: TestC (0.00s)\n--- PASS: TestD (0.00s)\n--- FAIL: TestE (0.00s)\n"
	if got := condenseOutput(in, 1); got != in {
		t.Errorf("condenseOutput = %q, want it unchanged", got)
	}
}

func TestLeanOutputIsOptIn(t *testing.T) {
	const in = "\x1b[31mred\x1b[0m\n"
	for _, v := range []string{"", "0", "true", "yes"} {
		t.Setenv("WANCTL_MCP_LEAN", v)
		if got := leanOutput(in, 1); got != in {
			t.Errorf("WANCTL_MCP_LEAN=%q: leanOutput = %q, want the output untouched", v, got)
		}
	}
	t.Setenv("WANCTL_MCP_LEAN", "1")
	if got := leanOutput(in, 1); got != "red\n" {
		t.Errorf("WANCTL_MCP_LEAN=1: leanOutput = %q, want %q", got, "red\n")
	}
}

// Every way a command's output reaches the model is condensed under
// WANCTL_MCP_LEAN=1 — exec, a background job's poll, a workspace command —
// and each is judged by the exit code that came with it. Without the switch
// the model gets exactly what it got before.
func TestLeanOutputOnEveryExecPath(t *testing.T) {
	h, access, target := hostedDevice(t)
	sid := openSession(t, h, access)
	script := filepath.Join(t.TempDir(), "noisy.sh")
	if err := os.WriteFile(script, []byte(`printf '\033[1;32mbuilding\033[0m\n'
for i in 1 2 3 4 5 6; do echo "--- PASS: TestOK$i (0.00s)"; done
echo "--- FAIL: TestBroken (0.00s)"
for i in 1 2 3 4 5 6; do echo waiting; done
printf '10%%\r60%%\r100%%\n'
exit 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	command := "sh " + script
	run := func() map[string]string {
		t.Helper()
		got := map[string]string{}
		got["wanctl_exec"], _ = toolCall(t, h, access, sid, "wanctl_exec", map[string]any{"target": target, "command": command, "oneshot": true})

		text, _ := toolCall(t, h, access, sid, "wanctl_exec_async", map[string]any{"target": target, "command": command})
		job := strings.TrimSpace(strings.TrimPrefix(text, "job_id: "))
		for deadline := time.Now().Add(30 * time.Second); !strings.HasPrefix(text, "state: done"); {
			if time.Now().After(deadline) {
				t.Fatal("background job did not finish")
			}
			text, _ = toolCall(t, h, access, sid, "wanctl_exec_poll", map[string]any{"target": target, "job_id": job})
		}
		got["wanctl_exec_poll"] = text

		text, bad := toolCall(t, h, access, sid, "wanctl_workspace", map[string]any{"action": "enter", "target": target, "root": t.TempDir()})
		if bad {
			t.Fatal(text)
		}
		ref := parseWorkspaceText(t, text)["workspace"].(string)
		defer toolCall(t, h, access, sid, "wanctl_workspace", map[string]any{"action": "exit", "workspace": ref})
		text, _ = toolCall(t, h, access, sid, "wanctl_exec", map[string]any{"workspace": ref, "command": command})
		for deadline := time.Now().Add(30 * time.Second); parseWorkspaceText(t, text)["done"] != true; {
			if time.Now().After(deadline) {
				t.Fatal("workspace command did not finish")
			}
			text, _ = toolCall(t, h, access, sid, "wanctl_exec_poll", map[string]any{"workspace": ref, "job_id": parseWorkspaceText(t, text)["request_id"]})
		}
		got["workspace"] = text
		return got
	}

	t.Setenv("WANCTL_MCP_LEAN", "")
	for path, text := range run() {
		for _, want := range []string{`\x1b[1;32mbuilding`, "--- PASS: TestOK1 (0.00s)", strings.Repeat("waiting\n", 6), "10%\r60%\r100%\n"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s without WANCTL_MCP_LEAN = %q, want it to carry %q as the device sent it", path, text, want)
			}
		}
	}

	t.Setenv("WANCTL_MCP_LEAN", "1")
	const condensed = "building\n" +
		"[... 6 passing-test lines omitted ...]\n" +
		"--- FAIL: TestBroken (0.00s)\n" +
		"waiting\n[... previous line repeated 5 more times ...]\n" +
		"100%\n"
	for path, text := range run() {
		if !strings.Contains(text, condensed) || strings.Contains(text, `\x1b`) || strings.Contains(text, "TestOK1") {
			t.Errorf("%s with WANCTL_MCP_LEAN=1 = %q, want the output condensed to %q", path, text, condensed)
		}
	}
}
