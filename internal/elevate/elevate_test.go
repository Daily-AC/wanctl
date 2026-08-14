package elevate

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeChannel counts probes so the cache can be asserted, and records what it
// was asked to run.
type fakeChannel struct {
	kind    Kind
	ok      bool
	reason  string
	probes  int
	ran     []string
	runCode int
	runErr  error
}

func (f *fakeChannel) Kind() Kind { return f.kind }

func (f *fakeChannel) Probe(context.Context) Status {
	f.probes++
	if !f.ok {
		return Status{Available: false, Reason: f.reason}
	}
	return Status{Available: true, Detail: "uid=0(root)"}
}

func (f *fakeChannel) Run(_ context.Context, command, _ string, out io.Writer) (int, error) {
	f.ran = append(f.ran, command)
	fmt.Fprintf(out, "ran on %s", f.kind)
	return f.runCode, f.runErr
}

func (f *fakeChannel) Close() error { return nil }

func TestSelectRefusesWhenElevationIsSwitchedOff(t *testing.T) {
	su := &fakeChannel{kind: KindSu, ok: true}
	m := NewManager(false, "turn on 提权通道 in the wanctl app", su)

	_, _, err := m.Select(context.Background(), "")
	if err == nil {
		t.Fatal("Select succeeded with elevation disabled")
	}
	if !strings.Contains(err.Error(), "提权通道") {
		t.Fatalf("error = %q, want it to say how to switch elevation on", err)
	}
	if su.probes != 0 {
		t.Fatalf("probed %d times while disabled, want 0", su.probes)
	}
}

func TestSelectPrefersProbeOrderNotArgumentOrder(t *testing.T) {
	// Channels are registered adb-first here; su must still win, because su is
	// the only one that survives a reboot unattended.
	adb := &fakeChannel{kind: KindADB, ok: true}
	su := &fakeChannel{kind: KindSu, ok: true}
	m := NewManager(true, "", adb, su)

	c, _, err := m.Select(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind() != KindSu {
		t.Fatalf("selected %s, want su", c.Kind())
	}
}

func TestSelectFallsThroughToTheNextAvailableChannel(t *testing.T) {
	su := &fakeChannel{kind: KindSu, ok: false, reason: "not rooted"}
	adb := &fakeChannel{kind: KindADB, ok: true}
	m := NewManager(true, "", su, adb)

	c, st, err := m.Select(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind() != KindADB {
		t.Fatalf("selected %s, want adb", c.Kind())
	}
	if !st.Available {
		t.Fatal("status reports unavailable for the selected channel")
	}
}

func TestPinnedChannelNeverSilentlyDowngrades(t *testing.T) {
	// The point of --via: asking for root and getting uid 2000 instead would be
	// a wrong answer that looks like a right one.
	su := &fakeChannel{kind: KindSu, ok: false, reason: "no su binary found"}
	adb := &fakeChannel{kind: KindADB, ok: true}
	m := NewManager(true, "", su, adb)

	_, _, err := m.Select(context.Background(), KindSu)
	if err == nil {
		t.Fatal("Select fell back to another channel after su was pinned")
	}
	if !strings.Contains(err.Error(), "no su binary found") {
		t.Fatalf("error = %q, want the pinned channel's own reason", err)
	}
	if len(adb.ran) != 0 {
		t.Fatal("a pinned su request reached the adb channel")
	}
}

func TestPinningAChannelThisBuildLacksIsDistinctFromItBeingUnavailable(t *testing.T) {
	// "not built in" and "built in but unavailable" send the user to different
	// places, so they must not collapse into one message.
	m := NewManager(true, "", &fakeChannel{kind: KindSu, ok: true})
	_, _, err := m.Select(context.Background(), KindADB)
	if err == nil {
		t.Fatal("Select accepted a channel this build does not have")
	}
	if !strings.Contains(err.Error(), "not built into this agent") {
		t.Fatalf("error = %q, want it to say the channel is not built in", err)
	}
}

func TestSelectReportsEveryReasonWhenNothingIsAvailable(t *testing.T) {
	m := NewManager(true, "",
		&fakeChannel{kind: KindSu, ok: false, reason: "not rooted"},
		&fakeChannel{kind: KindADB, ok: false, reason: "wireless debugging is off"},
	)
	_, _, err := m.Select(context.Background(), "")
	if err == nil {
		t.Fatal("Select succeeded with no channel available")
	}
	for _, want := range []string{"not rooted", "wireless debugging is off"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to include %q", err, want)
		}
	}
}

func TestProbeResultIsCachedThenExpires(t *testing.T) {
	su := &fakeChannel{kind: KindSu, ok: true}
	m := NewManager(true, "", su)
	now := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if _, _, err := m.Select(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
	}
	if su.probes != 1 {
		t.Fatalf("probed %d times within the TTL, want 1 (a consent dialog per command is not acceptable)", su.probes)
	}

	now = now.Add(DefaultProbeTTL + time.Second)
	if _, _, err := m.Select(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if su.probes != 2 {
		t.Fatalf("probed %d times after the TTL expired, want 2 (turning on wireless debugging must take effect without an agent restart)", su.probes)
	}
}

func TestRunReportsTheChannelThatActuallyRan(t *testing.T) {
	su := &fakeChannel{kind: KindSu, ok: false, reason: "not rooted"}
	adb := &fakeChannel{kind: KindADB, ok: true, runCode: 7}
	m := NewManager(true, "", su, adb)

	var sb strings.Builder
	via, code, err := m.Run(context.Background(), "", "id", "", &sb)
	if err != nil {
		t.Fatal(err)
	}
	if via != KindADB {
		t.Fatalf("Run reported %s, want the channel that ran (adb)", via)
	}
	if code != 7 {
		t.Fatalf("exit = %d, want the channel's own code 7", code)
	}
	if sb.String() != "ran on adb" {
		t.Fatalf("output = %q", sb.String())
	}
}

func TestSetEnabledFalseClearsTheProbeCache(t *testing.T) {
	// Otherwise switching elevation off and back on would keep answering from
	// probes taken while it was allowed.
	su := &fakeChannel{kind: KindSu, ok: true}
	m := NewManager(true, "", su)
	if _, _, err := m.Select(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	m.SetEnabled(false, "switched off")
	m.SetEnabled(true, "")
	if _, _, err := m.Select(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if su.probes != 2 {
		t.Fatalf("probes = %d, want 2 (the cache must not survive the switch)", su.probes)
	}
}

func TestStatusesExplainRatherThanErrorWhenNothingWorks(t *testing.T) {
	m := NewManager(true, "",
		&fakeChannel{kind: KindADB, ok: false, reason: "wireless debugging is off"},
		&fakeChannel{kind: KindSu, ok: false, reason: "not rooted"},
	)
	got := m.Statuses(context.Background())
	if len(got) != 2 {
		t.Fatalf("got %d statuses, want 2", len(got))
	}
	if got[0].Kind != KindSu {
		t.Fatalf("statuses[0] = %s, want probe order (su first)", got[0].Kind)
	}
	for _, st := range got {
		if st.Reason == "" {
			t.Fatalf("%s reported unavailable with no reason", st.Kind)
		}
	}
}

func TestStatusesSayWhenElevationIsOffRatherThanProbing(t *testing.T) {
	su := &fakeChannel{kind: KindSu, ok: true}
	m := NewManager(false, "turn on 提权通道 in the wanctl app", su)
	got := m.Statuses(context.Background())
	if len(got) != 1 || got[0].Available {
		t.Fatalf("statuses = %+v, want the channel reported unavailable", got)
	}
	if !strings.Contains(got[0].Reason, "提权通道") {
		t.Fatalf("reason = %q, want the switch instruction", got[0].Reason)
	}
	if su.probes != 0 {
		t.Fatal("probed a channel while elevation was switched off")
	}
}

func TestParseKind(t *testing.T) {
	for _, in := range []string{"su", "SU", "adb"} {
		if _, err := ParseKind(in); err != nil {
			t.Fatalf("ParseKind(%q) = %v", in, err)
		}
	}
	for _, in := range []string{"", "root", "magisk", "none"} {
		if _, err := ParseKind(in); err == nil {
			t.Fatalf("ParseKind(%q) accepted an unknown channel", in)
		}
	}
}

func TestConfigureIsOffByDefaultAndAndroidOnly(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	// Android with the switch off: disabled, and the message names the switch.
	m := Configure("android", t.TempDir(), env(nil))
	if m.Enabled() {
		t.Fatal("elevation is on by default; it must not be")
	}
	if _, _, err := m.Select(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), SwitchEnv) {
		t.Fatalf("error = %v, want it to name %s", err, SwitchEnv)
	}
	// Android with the switch on: enabled, su present.
	m = Configure("android", t.TempDir(), env(map[string]string{SwitchEnv: "1"}))
	if !m.Enabled() {
		t.Fatal("switch on did not enable elevation")
	}
	// Every other platform: off regardless of the switch (ADR 0004).
	for _, goos := range []string{"linux", "darwin", "windows"} {
		m := Configure(goos, t.TempDir(), env(map[string]string{SwitchEnv: "1"}))
		if m.Enabled() {
			t.Fatalf("%s: elevation enabled off-Android", goos)
		}
	}
}
