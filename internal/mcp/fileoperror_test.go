package mcp

import (
	"strings"
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/protocol"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

func errorText(t *testing.T, res *mcpapi.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("result is not an error: %+v", res.Content)
	}
	return res.Content[0].(mcpapi.TextContent).Text
}

// The file tools have to tell these two apart, because the actions they call
// for are opposite: one says the device cannot do this at all, the other says it
// may already have done it.
func TestFileErrorsDistinguishLostResultsFromOldAgents(t *testing.T) {
	sess := &localFsSession{}

	lost := errorText(t, fileOpErrorResult(sess, &client.ResultLostError{
		Kind: protocol.KindFileEdit, Path: "/etc/app.conf", Target: "lab",
	}))
	for _, want := range []string{
		"result unknown: the connection dropped after the request was sent",
		"read the file and compare sha256 before retrying",
		"wanctl_read",
		"Do NOT simply retry",
	} {
		if !strings.Contains(lost, want) {
			t.Errorf("lost-result message does not say %q:\n%s", want, lost)
		}
	}
	if strings.Contains(lost, "wanctl update") {
		t.Errorf("a lost result told the caller to update the agent:\n%s", lost)
	}

	unsupported := errorText(t, fileOpErrorResult(sess, &client.UnsupportedError{
		Kind: protocol.KindFileWrite, Target: "lab",
	}))
	if !strings.Contains(unsupported, "does not support write") || !strings.Contains(unsupported, "wanctl update") {
		t.Errorf("unsupported message lost the update instruction:\n%s", unsupported)
	}
	if strings.Contains(unsupported, "result unknown") {
		t.Errorf("an unsupported agent was reported as an unknown result:\n%s", unsupported)
	}
}

// A lost READ has nothing to inspect afterwards and nothing to undo, so it says
// to retry. Appending "do not simply retry, read the file first" there would
// contradict the sentence in front of it.
func TestALostReadIsNotToldToCheckTheFile(t *testing.T) {
	sess := &localFsSession{}

	read := errorText(t, fileOpErrorResult(sess, &client.ResultLostError{
		Kind: protocol.KindFileRead, Path: "/etc/hosts",
	}))
	if !strings.Contains(read, "nothing was changed") || !strings.Contains(read, "retry") {
		t.Errorf("a lost read should say retrying is safe:\n%s", read)
	}
	if strings.Contains(read, "Do NOT simply retry") || strings.Contains(read, "compare the sha256") {
		t.Errorf("a lost read was told to go hash a file it never changed:\n%s", read)
	}

	// The mutating kinds keep the clause, which is the whole point of it.
	for _, kind := range []string{protocol.KindFileEdit, protocol.KindFileWrite} {
		got := errorText(t, fileOpErrorResult(sess, &client.ResultLostError{Kind: kind, Path: "/etc/app.conf"}))
		if !strings.Contains(got, "Do NOT simply retry") {
			t.Errorf("%s lost its check-the-file instruction:\n%s", kind, got)
		}
	}
}
