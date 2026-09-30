package protocol

import "time"

// ApprovalCard is what the portal pushes to the owner's approval phone
// (KindApprovalPush) and what the phone's app turns into a notification. One
// card id stands for one pending approval or pairing on one target device;
// later pushes with the same id update the same notification. See ADR 0015.
type ApprovalCard struct {
	ID    string `json:"id"`    // portal-issued; the only handle a decision may name
	State string `json:"state"` // ApprovalPending, ApprovalExpired, ApprovalDone, ApprovalTest

	Kind   string `json:"kind,omitempty"`    // policy kind ("exec", "read", …) or "pair"
	Device string `json:"device,omitempty"`  // the target device the request is for
	Peer   string `json:"peer,omitempty"`    // controller name as the device knows it
	PeerFP string `json:"peer_fp,omitempty"` // controller fingerprint
	Cmd    string `json:"cmd,omitempty"`     // policy.CommandLabel of the command
	Path   string `json:"path,omitempty"`
	Cwd    string `json:"cwd,omitempty"`

	Created time.Time `json:"created,omitempty"`
	Expires time.Time `json:"expires,omitempty"` // when the synchronous wait ends

	Result string `json:"result,omitempty"` // for ApprovalDone: one of the Result* values
}

// Card states.
const (
	ApprovalPending = "pending" // waiting; allow or deny reaches the request directly
	ApprovalExpired = "expired" // the wait ran out; allow installs a late grant
	ApprovalDone    = "done"    // final; Result says how it ended
	ApprovalTest    = "test"    // sent once when the phone is designated
)

// Final results carried by an ApprovalDone card.
const (
	ResultAllowed = "allowed" // the waiting request was allowed
	ResultDenied  = "denied"  // the owner refused it
	ResultGranted = "granted" // late approval: one retry within the grant window
	ResultHandled = "handled" // decided elsewhere (portal, terminal) or gone before the owner acted
	ResultGone    = "gone"    // too late: the request and any late path have lapsed
)

// LateGrantWindow is how long a late approval stays usable on the target
// device (ADR 0015, decision 2).
const LateGrantWindow = 30 * time.Minute
