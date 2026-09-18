package delegation

import (
	"testing"
	"time"
)

// The ledger allowance is the one number that used to encode "a grant lives at
// most an hour". Short grants must come out exactly as they did before, and a
// grant that lasts a day must not spend its whole allowance in the first hour.
func TestMaxJobsScalesWithApprovedHoursAndKeepsTheOldFloor(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		want    int
	}{
		{1, 64},
		{15, 64},
		{59, 64},
		{60, 64},
		{61, 128},
		{120, 128},
		{121, 192},
		{240, 256},
		{1440, 1536},
		{MaxGrantMinutes, 1536},
	} {
		if got := MaxJobs(time.Duration(tc.minutes) * time.Minute); got != tc.want {
			t.Errorf("MaxJobs(%d min) = %d, want %d", tc.minutes, got, tc.want)
		}
	}
	// A grant with no recorded duration still gets the floor rather than zero:
	// refusing every operation would be a worse failure than allowing 64.
	if got := MaxJobs(0); got != MinJobsPerGrant {
		t.Errorf("MaxJobs(0) = %d, want the floor %d", got, MinJobsPerGrant)
	}
	if got := MaxJobs(-time.Hour); got != MinJobsPerGrant {
		t.Errorf("MaxJobs(-1h) = %d, want the floor %d", got, MinJobsPerGrant)
	}
}

// The three durations downstream of the grant ceiling are relationships, not
// independent settings: a ticket has to outlive the grant it carries, and a
// deleted record must never be revivable by a ticket that is still valid.
func TestDurationsDerivedFromTheGrantCeiling(t *testing.T) {
	if TicketLifetime != RequestWindow+MaxGrantMinutes*time.Minute {
		t.Fatalf("ticket envelope %v does not cover a %d-minute grant approved %v after issue",
			TicketLifetime, MaxGrantMinutes, RequestWindow)
	}
	// Retention counts from the request row's created_at, which can be a whole
	// RequestWindow after the ticket was issued.
	if MinRetention <= TicketLifetime-RequestWindow {
		t.Fatalf("retention floor %v is not strictly past the last moment a ticket can name a row", MinRetention)
	}
}

func TestGrantedDurationOnlyExistsForAnApprovedRequest(t *testing.T) {
	decided := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	expires := decided.Add(4 * time.Hour)
	if got := (Request{DecidedAt: &decided, ExpiresAt: &expires}).GrantedDuration(); got != 4*time.Hour {
		t.Fatalf("granted duration = %v, want 4h", got)
	}
	if got := (Request{ExpiresAt: &expires}).GrantedDuration(); got != 0 {
		t.Fatalf("a request that was never decided reports %v", got)
	}
	if got := (Request{DecidedAt: &decided}).GrantedDuration(); got != 0 {
		t.Fatalf("a rejected request reports %v", got)
	}
}
