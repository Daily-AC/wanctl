package elevate

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/adb"
	"wanctl/internal/adb/adbtest"
)

func TestADBStreamDoesNotBlockRun(t *testing.T) {
	server := adbtest.Start(t, func(_ context.Context, cmd string, in io.Reader, out, stderr io.Writer) int {
		if cmd == "id" {
			io.WriteString(out, "uid=2000(shell)\n")
			return 0
		}
		if cmd == "normal" {
			io.WriteString(out, "command completed\n")
			return 0
		}
		io.Copy(out, in)
		return 0
	})
	a := NewADB(t.TempDir(), "test")
	a.ports = func() ([]int, string) { return []int{1234}, "test" }
	var dials atomic.Int32
	a.dial = func(ctx context.Context, _ string, k *adb.Key) (shellConn, error) {
		dials.Add(1)
		return adb.Dial(ctx, server.Addr, k, nil)
	}
	defer a.Close()
	m := NewManager(true, "", a)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := m.Stream(ctx, "", "engine", "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var out strings.Builder
	_, code, err := m.Run(ctx, "", "normal", "", &out)
	if err != nil || code != 0 || out.String() != "command completed\n" {
		t.Fatalf("Run while stream open: %d %q %v", code, out.String(), err)
	}
	if dials.Load() != 2 {
		t.Fatalf("dials %d; expected shared Run and dedicated stream", dials.Load())
	}
	io.WriteString(stream.Stdin(), "hello\n")
	stream.Stdin().Close()
	data, err := io.ReadAll(stream.Stdout())
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("stream %q %v", data, err)
	}
	if code, err = stream.Wait(); err != nil || code != 0 {
		t.Fatalf("Wait %d %v", code, err)
	}
}

type fakeStreamingChannel struct {
	fakeChannel
	opened  int
	fail    error
	command string
}

func (c *fakeStreamingChannel) Stream(_ context.Context, cmd, _ string) (Stream, error) {
	c.opened++
	c.command = cmd
	if c.fail != nil {
		return nil, c.fail
	}
	return &finishedStream{}, nil
}
func (c *fakeStreamingChannel) ShellStream(ctx context.Context, cmd, cwd string) (Stream, error) {
	return c.Stream(ctx, cmd, cwd)
}

type finishedStream struct{}

func (*finishedStream) Stdin() io.WriteCloser { return nopInput{} }
func (*finishedStream) Stdout() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (*finishedStream) Stderr() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (*finishedStream) Wait() (int, error)    { return 0, nil }
func (*finishedStream) Close() error          { return nil }

type nopInput struct{}

func (nopInput) Write(p []byte) (int, error) { return len(p), nil }
func (nopInput) Close() error                { return nil }
func TestStreamSelectionSwitchPinningAndNoDowngrade(t *testing.T) {
	su := &fakeStreamingChannel{fakeChannel: fakeChannel{kind: KindSu, ok: true}, fail: errors.New("su stream failed")}
	adb := &fakeStreamingChannel{fakeChannel: fakeChannel{kind: KindADB, ok: true}}
	m := NewManager(false, "turn on 提权通道 in the wanctl app", adb, su)
	if _, err := m.Stream(context.Background(), "", "x", ""); err == nil || !strings.Contains(err.Error(), "提权通道") {
		t.Fatal(err)
	}
	if su.probes+adb.probes != 0 {
		t.Fatal("disabled stream probed")
	}
	m.SetEnabled(true, "")
	if _, err := m.ShellStream(context.Background(), "", "engine", ""); err != su.fail {
		t.Fatal(err)
	}
	if su.opened != 1 || adb.opened != 0 || !strings.Contains(su.command, `"$(id -u)" != 2000`) {
		t.Fatal("order, uid guard or fallback")
	}
	if _, err := m.Stream(context.Background(), KindADB, "pinned", ""); err != nil {
		t.Fatal(err)
	}
	if adb.opened != 1 {
		t.Fatal("pin ignored")
	}
	off := &fakeStreamingChannel{fakeChannel: fakeChannel{kind: KindSu, reason: "not rooted"}}
	m = NewManager(true, "", off, adb)
	if _, err := m.Stream(context.Background(), KindSu, "x", ""); err == nil {
		t.Fatal("unavailable pinned stream accepted")
	}
	if adb.opened != 1 {
		t.Fatal("silently downgraded")
	}
}
func TestADBStreamRetainsLinkFailureSentences(t *testing.T) {
	for _, tc := range []struct {
		err        error
		want, link string
	}{
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, "turn on wireless debugging", LinkNoPort},
		{adb.ErrKeyRejected, "adbd does not accept wanctl's key", LinkUnpaired},
		{adb.ErrPublicKeyPending, "allow wanctl's key", LinkConfirm},
	} {
		a := newTestADB(t, 1234, nil, tc.err)
		_, err := a.Stream(context.Background(), "engine", "")
		if err == nil || !strings.Contains(err.Error(), tc.want) || linkOf(err) != tc.link {
			t.Fatalf("%v link=%s", err, linkOf(err))
		}
	}
}
func TestSuStreamsPipesEOFAndShellIdentityArguments(t *testing.T) {
	for _, aosp := range []bool{false, true} {
		t.Run(map[bool]string{false: "Magisk", true: "AOSP"}[aosp], func(t *testing.T) {
			body := `if [ "$1" = shell ] && [ "$2" = -c ]; then exec /bin/sh -c "$3"; fi; exit 19`
			form := suFormDashC
			if aosp {
				body = `if [ "$1" = shell ] && [ "$2" = /bin/sh ] && [ "$3" = -c ]; then exec /bin/sh -c "$4"; fi; exit 19`
				form = suFormUserSh
			}
			su := stubSu(t, body)
			su.form = form
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s, err := su.ShellStream(ctx, "cat; echo diagnostic >&2; exit 42", "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			io.WriteString(s.Stdin(), "body")
			s.Stdin().Close()
			b, _ := io.ReadAll(s.Stdout())
			e, _ := io.ReadAll(s.Stderr())
			if string(b) != "body" || string(e) != "diagnostic\n" {
				t.Fatalf("pipes: %q %q", b, e)
			}
			if code, err := s.Wait(); err != nil || code != 42 {
				t.Fatalf("exit %d %v", code, err)
			}
		})
	}
}

func TestStreamFailureUpdatesADBLinkCache(t *testing.T) {
	ch := &fakeStreamingChannel{fakeChannel: fakeChannel{kind: KindADB, ok: true}, fail: &linkError{link: LinkUnpaired, msg: "pair the device again"}}
	m := NewManager(true, "", ch)
	_, err := m.Stream(context.Background(), KindADB, "engine", "")
	if err == nil {
		t.Fatal("expected stream failure")
	}
	statuses := m.Statuses(context.Background())
	if len(statuses) != 1 || statuses[0].Available || statuses[0].Link != LinkUnpaired || statuses[0].Reason != "pair the device again" {
		t.Fatalf("%+v", statuses)
	}
}

func TestShellStreamRefusesWrongUIDRatherThanRunningEngine(t *testing.T) {
	fakeRootID(t)
	su := stubSu(t, `case "$1" in
 -c) echo 'uid=0(root)';;
 shell) shift; exec /bin/sh "$@";;
 *) exit 19;;
 esac`)
	m := NewManager(true, "", su)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := m.ShellStream(ctx, KindSu, "echo ENGINE_RAN", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Stdin().Close()
	out, _ := io.ReadAll(s.Stdout())
	stderr, _ := io.ReadAll(s.Stderr())
	code, err := s.Wait()
	if string(out) != "" || code != 1 || err != nil || !strings.Contains(string(stderr), "requires the shell user (uid 2000)") {
		t.Fatalf("wrong identity admitted: %d %q %q %v", code, out, stderr, err)
	}
}
