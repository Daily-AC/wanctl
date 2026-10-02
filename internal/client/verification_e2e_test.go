package client

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// verificationRig is the exec/push/pull fixture with the device's own config
// directory kept in hand: the certificate-change case below restarts the same
// installation — same device ID, new key pair — the way a reinstall does.
type verificationRig struct {
	client    *Client
	ctx       context.Context
	base      string
	deviceDir string
	ag        *agent.Agent
}

func newVerificationRig(t *testing.T) *verificationRig {
	t.Helper()
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	r := &verificationRig{base: base, deviceDir: t.TempDir()}
	var cancel context.CancelFunc
	r.ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() {
		if r.ag != nil {
			r.ag.Close()
		}
	})
	r.startAgent(t)

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", base)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "ws")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	r.client = c
	r.waitForDevice(t, "")
	return r
}

func (r *verificationRig) startAgent(t *testing.T) {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", r.deviceDir)
	ag, err := agent.New(agent.Options{
		RelayURL: r.base, Token: "tok", Name: "home-pc",
		AutoYes: true, Mode: policy.ModeBypass, Version: "v0.0.0-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	r.ag = ag
	go ag.Run(r.ctx)
}

// waitForDevice blocks until this controller can read a certificate from the
// device and returns its fingerprint. When differentFrom is non-empty it waits
// for a certificate that is not that one, which is how the reinstalled device
// below is recognised.
func (r *verificationRig) waitForDevice(t *testing.T, differentFrom string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, cancel := context.WithTimeout(r.ctx, 2*time.Second)
		_, fp, err := r.client.present(probe, "home-pc")
		cancel()
		if err == nil && (differentFrom == "" || fp != differentFrom) {
			return fp
		}
		if time.Now().After(deadline) {
			t.Fatalf("device home-pc never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A reinstall keeps the installation's device ID and mints a new key pair, so
// the name still resolves and only the certificate moved.
func (r *verificationRig) reinstallDevice(t *testing.T) {
	t.Helper()
	before := r.waitForDevice(t, "")
	r.ag.Close()
	for _, name := range []string{"cert.pem", "key.pem"} {
		if err := os.Remove(filepath.Join(r.deviceDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	r.startAgent(t)
	r.waitForDevice(t, before)
}

// The whole point of the change: first contact hands the human nine digits to
// compare instead of forty-three characters to transcribe, and the code is
// derived from the certificate the dial presented — the same derivation the
// device runs on its own certificate. What a human reads off the device is
// therefore checkable here, and a code that did not come off that device pins
// nothing.
func TestFirstContactOffersAVerificationCodeOnlyTheDeviceCanAnswer(t *testing.T) {
	r := newVerificationRig(t)
	_, _, pairErr := r.client.Pair(r.ctx, "home-pc")
	var first *TrustRequiredError
	if !errors.As(pairErr, &first) {
		t.Fatalf("first contact: want TrustRequiredError, got %v", pairErr)
	}
	if _, err := transport.NormalizeVerifyNumber(first.Number); err != nil {
		t.Fatalf("verification number %q: %v", first.Number, err)
	}
	// What `wanctl verify <number>` prints on the device, computed from the
	// certificate the dial was shown.
	onDevice, err := transport.VerifyCode(first.Fingerprint, first.Number)
	if err != nil {
		t.Fatal(err)
	}
	// The message a newcomer reads has to carry the next command, using the
	// short form.
	text := pairErr.Error()
	for _, want := range []string{
		"DEVICE IDENTITY CONFIRMATION REQUIRED",
		"verification number:  " + transport.GroupDigits(first.Number),
		"wanctl verify " + first.Number,
		"--fingerprint \"" + first.Fingerprint + "\" --number " + first.Number,
		"same refusal",
		"--number " + first.Number + " --code <the code the device shows>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("first-contact message is missing %q:\n%s", want, text)
		}
	}

	assertNoDerivedCode(t, text, onDevice)

	// A code the device never printed is refused, and pins nothing.
	other := "000000000"
	if onDevice == other {
		other = "111111111"
	}
	wrong := &TrustChallenge{Target: first.Target, Fingerprint: first.Fingerprint, Number: first.Number}
	if _, _, err := r.client.ConfirmTrust(r.ctx, wrong, other, false); err == nil {
		t.Fatal("a code that did not come off the device was accepted")
	} else {
		var mismatch *VerifyCodeMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("wrong code: want VerifyCodeMismatchError, got %v", err)
		}
		assertNoDerivedCode(t, err.Error(), onDevice)
		if !strings.Contains(err.Error(), "VERIFICATION CODE MISMATCH") {
			t.Errorf("refusal is not matchable:\n%s", err)
		}
	}
	if _, pinned := r.client.Pinned(first.Target); pinned {
		t.Fatal("a refused verification pinned the device anyway")
	}

	// The code read off the device pins it, and the device is then drivable.
	canonical, pinned, err := r.client.ConfirmTrust(r.ctx, wrong, onDevice, false)
	if err != nil {
		t.Fatalf("confirm with the device's own code: %v", err)
	}
	if pinned != first.Fingerprint {
		t.Fatalf("pinned %s, want the certificate the dial presented (%s)", pinned, first.Fingerprint)
	}
	if stored, ok := r.client.Pinned(canonical); !ok || stored.Fingerprint != first.Fingerprint {
		t.Fatalf("the pin that was recorded is %+v (ok=%v)", stored, ok)
	}
	var out, errOut strings.Builder
	if _, err := r.client.ExecTo(r.ctx, ExecRequest{Target: "home-pc", Command: "echo pinned"}, &out, &errOut); err != nil {
		t.Fatalf("exec after a verified pin: %v", err)
	}
	if !strings.Contains(out.String(), "pinned") {
		t.Fatalf("exec output = %q (stderr %q)", out.String(), errOut.String())
	}
}

// The identity being checked is the certificate, not the spelling of the
// target. A challenge that carries the fingerprint the human compared has to
// confirm through whatever name the caller typed, or a legitimate pin would
// fail with an identity alarm whose two fingerprint lines print the same value.
func TestAChallengeConfirmsThroughANonCanonicalTarget(t *testing.T) {
	r := newVerificationRig(t)
	_, _, err := r.client.Pair(r.ctx, "home-pc")
	var first *TrustRequiredError
	if !errors.As(err, &first) {
		t.Fatalf("first contact: want TrustRequiredError, got %v", err)
	}
	ch := &TrustChallenge{Target: "home-pc", Fingerprint: first.Fingerprint, Number: first.Number}
	onDevice, err := transport.VerifyCode(first.Fingerprint, first.Number)
	if err != nil {
		t.Fatal(err)
	}
	canonical, pinned, err := r.client.ConfirmTrust(r.ctx, ch, onDevice, false)
	if err != nil {
		t.Fatalf("confirm through a typed name: %v", err)
	}
	if pinned != first.Fingerprint || canonical == "" {
		t.Fatalf("pinned %q/%s, want the presented certificate under a canonical name", canonical, pinned)
	}
}

// The two-process flow must stay bound to the certificate from the refusal.
// The challenge binds the identity it was issued for, so a device that answers
// with a different certificate after the human has compared is refused as an
// identity change rather than pinned as if the check had passed.
func TestConfirmingADeviceThatChangedAfterTheNumberWasIssuedPinsNothing(t *testing.T) {
	r := newVerificationRig(t)
	ch := refusalChallenge(t, r)
	onDevice, err := transport.VerifyCode(ch.Fingerprint, ch.Number)
	if err != nil {
		t.Fatal(err)
	}
	r.reinstallDevice(t)
	// Even a correct code for the new certificate cannot replace the original.
	onDevice, err = transport.VerifyCode(r.waitForDevice(t, ""), ch.Number)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = r.client.ConfirmTrust(r.ctx, ch, onDevice, false)
	var changed *VerifiedIdentityChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("reinstalled device: want VerifiedIdentityChangedError, got %v", err)
	}
	if !strings.Contains(err.Error(), "DEVICE IDENTITY MISMATCH") {
		t.Errorf("refusal is not matchable:\n%s", err)
	}
	if _, pinned := r.client.Pinned(ch.Target); pinned {
		t.Fatal("the identity that was verified is not the one answering, yet it was pinned")
	}
}

// A carried number without its original certificate must never reach a dial.
func TestACarriedNumberWithoutAFingerprintPinsNothing(t *testing.T) {
	r := newVerificationRig(t)
	ch := refusalChallenge(t, r)
	onDevice, err := transport.VerifyCode(ch.Fingerprint, ch.Number)
	if err != nil {
		t.Fatal(err)
	}
	ch.Fingerprint = ""
	if _, _, err := r.client.ConfirmTrust(r.ctx, ch, onDevice, false); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("number without fingerprint: %v", err)
	}
	if _, pinned := r.client.Pinned(ch.Target); pinned {
		t.Fatal("missing fingerprint pinned a device")
	}
}

func refusalChallenge(t *testing.T, r *verificationRig) *TrustChallenge {
	t.Helper()
	_, _, err := r.client.Pair(r.ctx, "home-pc")
	var first *TrustRequiredError
	if !errors.As(err, &first) {
		t.Fatalf("first contact: %v", err)
	}
	return &TrustChallenge{Target: first.Target, Fingerprint: first.Fingerprint, Number: first.Number}
}

func assertNoDerivedCode(t *testing.T, text, code string) {
	t.Helper()
	for _, forbidden := range []string{code, transport.GroupDigits(code), "this controller derived:"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("controller disclosed derived code %q: %s", forbidden, text)
		}
	}
}

// The number a human types in is digits, not formatting: the grouped form on
// screen must be the same number as the ungrouped one in the command.
func TestThePrintedNumberIsTheGroupedFormOfTheCommandNumber(t *testing.T) {
	r := newVerificationRig(t)
	ch := refusalChallenge(t, r)
	grouped := transport.GroupDigits(ch.Number)
	if strings.ReplaceAll(grouped, " ", "") != ch.Number {
		t.Fatalf("printed number %q is not %q grouped", grouped, ch.Number)
	}
	if _, err := strconv.Atoi(ch.Number); err != nil {
		t.Fatalf("number %q is not digits: %v", ch.Number, err)
	}
	if !regexp.MustCompile(`^\d{6}$`).MatchString(ch.Number) {
		t.Fatalf("number %q is not six digits", ch.Number)
	}
}

// No initialized transport is needed to reject an unbound challenge. Moving
// validation past the dial would make this dereference the absent identity.
func TestConfirmTrustRejectsMissingFingerprintBeforeDial(t *testing.T) {
	c := &Client{}
	for _, fp := range []string{"", "not-a-fingerprint"} {
		_, _, err := c.ConfirmTrust(context.Background(), &TrustChallenge{Target: "alice/device", Fingerprint: fp, Number: "482913"}, "771204638", false)
		if err == nil || !strings.Contains(err.Error(), "fingerprint") {
			t.Fatalf("fingerprint %q: %v", fp, err)
		}
	}
}
