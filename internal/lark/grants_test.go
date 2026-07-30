package lark

import (
	"errors"
	"testing"
	"time"
)

func TestGrantsConsume(t *testing.T) {
	grants, _ := testGrants()
	issued, err := grants.Issue("team", "macbox", "pending-1", "", "oc-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := grants.Consume(issued.Nonce, "oc-owner", "event-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != issued {
		t.Fatalf("Consume = %#v, want %#v", got, issued)
	}
}

func TestGrantsBindChatAfterIssuingNonce(t *testing.T) {
	grants, _ := testGrants()
	issued, err := grants.Issue("team", "macbox", "pending-1", "", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Consume(issued.Nonce, "oc-owner", "event-before-bind"); !errors.Is(err, ErrGrantUnbound) {
		t.Fatalf("Consume error = %v, want ErrGrantUnbound", err)
	}
	bound, err := grants.BindChat(issued.Nonce, "oc-owner")
	if err != nil {
		t.Fatal(err)
	}
	if bound.ChatID != "oc-owner" {
		t.Fatalf("BindChat ChatID = %q", bound.ChatID)
	}
	if _, err := grants.BindChat(issued.Nonce, "oc-forwarded"); !errors.Is(err, ErrChatMismatch) {
		t.Fatalf("rebind error = %v, want ErrChatMismatch", err)
	}
	if _, err := grants.Consume(issued.Nonce, "oc-owner", "event-after-bind"); err != nil {
		t.Fatal(err)
	}
}

func TestGrantsRejectChatMismatch(t *testing.T) {
	grants, _ := testGrants()
	issued, err := grants.Issue("team", "macbox", "pending-1", "", "oc-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Consume(issued.Nonce, "oc-forwarded", "event-wrong-chat"); !errors.Is(err, ErrChatMismatch) {
		t.Fatalf("Consume error = %v, want ErrChatMismatch", err)
	}
	if _, err := grants.Consume(issued.Nonce, "oc-owner", "event-correct-chat"); err != nil {
		t.Fatalf("chat mismatch consumed nonce: %v", err)
	}
}

func TestGrantsRejectConsumedNonce(t *testing.T) {
	grants, _ := testGrants()
	issued, err := grants.Issue("team", "macbox", "pending-1", "", "oc-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Consume(issued.Nonce, "oc-owner", "event-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Consume(issued.Nonce, "oc-owner", "event-2"); !errors.Is(err, ErrNonceConsumed) {
		t.Fatalf("Consume error = %v, want ErrNonceConsumed", err)
	}
}

func TestGrantsRejectExpiredNonceAndCleanItUp(t *testing.T) {
	grants, now := testGrants()
	issued, err := grants.Issue("team", "macbox", "", "SHA256:pair", "oc-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if _, err := grants.Consume(issued.Nonce, "oc-owner", "event-expired"); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("Consume error = %v, want ErrGrantExpired", err)
	}
	if len(grants.active) != 0 {
		t.Fatalf("expired active records = %d, want 0", len(grants.active))
	}
	if _, err := grants.Issue("team", "macbox", "pending-2", "", "oc-owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Consume(issued.Nonce, "oc-owner", "event-expired-again"); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("Consume after cleanup error = %v, want ErrGrantExpired", err)
	}
	*now = now.Add(expiredGrantRetention)
	if _, err := grants.Issue("team", "macbox", "pending-3", "", "oc-owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, exists := grants.expired[issued.Nonce]; exists {
		t.Fatal("expired tombstone was not cleaned after retention period")
	}
}

func TestGrantsRejectDuplicateEvent(t *testing.T) {
	grants, _ := testGrants()
	first, err := grants.Issue("team", "macbox", "pending-1", "", "oc-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := grants.Issue("team", "macbox", "pending-2", "", "oc-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Consume(first.Nonce, "oc-owner", "event-duplicate"); err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Consume(second.Nonce, "oc-owner", "event-duplicate"); !errors.Is(err, ErrEventDuplicate) {
		t.Fatalf("Consume error = %v, want ErrEventDuplicate", err)
	}
}

func testGrants() (*Grants, *time.Time) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	grants := NewGrants()
	grants.now = func() time.Time { return now }
	return grants, &now
}
