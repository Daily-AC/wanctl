package policy

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Approver decides a request that no rule already covers.
type Approver interface {
	Ask(req Request) Decision
}

// AllowApprover allows everything (used for bypass mode and tests).
type AllowApprover struct{}

func (AllowApprover) Ask(Request) Decision { return Decision{Allow: true} }

// DenyApprover denies everything (used when running unattended without a TTY).
type DenyApprover struct{}

func (DenyApprover) Ask(Request) Decision { return Decision{Allow: false} }

// ConsoleApprover prompts a human on the device console and reads y/a/g/n.
type ConsoleApprover struct {
	in  *bufio.Reader
	out io.Writer
}

// NewConsoleApprover builds a console approver over the given streams.
func NewConsoleApprover(in io.Reader, out io.Writer) *ConsoleApprover {
	return &ConsoleApprover{in: bufio.NewReader(in), out: out}
}

// Ask presents the request and maps the answer to a Decision:
//
//	y → allow once · a → allow + remember this dir · g → allow + remember global · n → deny
func (c *ConsoleApprover) Ask(req Request) Decision {
	fmt.Fprintf(c.out, "\n──────────────────────────────────────────────\n")
	switch req.Kind {
	case KindExec:
		fmt.Fprintf(c.out, "  Approve COMMAND from %s\n    cmd: %s\n", short(req.Peer), req.Cmd)
		if req.Cwd != "" {
			fmt.Fprintf(c.out, "    cwd: %s\n", req.Cwd)
		}
	case KindExecElevated:
		// Named differently on purpose. Someone skimming this prompt must not
		// mistake it for the ordinary one; the whole point of the separate
		// policy class is that this decision is a bigger one.
		fmt.Fprintf(c.out, "  Approve ELEVATED COMMAND from %s\n", short(req.Peer))
		fmt.Fprintf(c.out, "    cmd: %s\n", req.Cmd)
		if req.Via != "" {
			fmt.Fprintf(c.out, "    via: %s\n", req.Via)
		}
		if req.Cwd != "" {
			fmt.Fprintf(c.out, "    cwd: %s\n", req.Cwd)
		}
	case KindRead:
		fmt.Fprintf(c.out, "  Approve READ from %s\n    path: %s\n", short(req.Peer), req.Path)
	case KindWrite:
		fmt.Fprintf(c.out, "  Approve WRITE from %s\n    path: %s\n", short(req.Peer), req.Path)
	case KindLogs:
		fmt.Fprintf(c.out, "  Approve EVENT LOG READ from %s\n", short(req.Peer))
	}
	fmt.Fprintf(c.out, "  [y] once  [a] remember this dir  [g] remember global  [n] deny: ")
	line, _ := c.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return Decision{Allow: true}
	case "a":
		if req.Kind == KindLogs {
			return Decision{Allow: true}
		}
		return Decision{Allow: true, Remember: true, Scope: ScopeDir}
	case "g":
		return Decision{Allow: true, Remember: true, Scope: ScopeGlobal}
	default:
		return Decision{Allow: false}
	}
}

func short(s string) string {
	if len(s) > 25 {
		return s[:14] + "…" + s[len(s)-6:]
	}
	if s == "" {
		return "(unknown)"
	}
	return s
}
