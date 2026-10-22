package relay

import (
	"errors"
	"testing"
)

func TestDisabledAccountContactStore(t *testing.T) {
	p, _, _ := disableTestStore(t)
	_, _, err := p.IssueEmailConfirmation("github", "102", "bob", "bob@example.com", "/")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.SetAccountDisabled("bob", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.IssueEmailConfirmation("github", "102", "bob", "new@example.com", "/"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("send: %v", err)
	}
	if _, err := p.ContactEmail("github", "102"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("status: %v", err)
	}
	if err := p.CancelEmailConfirmations("github", "102"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, _, err := p.IssueEmailConfirmation("github", "new-applicant", "new", "new-user@example.com", "/"); err != nil {
		t.Fatalf("applicant send: %v", err)
	}
	if _, _, err := p.SetAccountDisabled("bob", false); err != nil {
		t.Fatal(err)
	}
	c, err := p.ContactEmail("github", "102")
	if err != nil || c.Pending != "bob@example.com" {
		t.Fatalf("pending link changed: %+v %v", c, err)
	}
}
