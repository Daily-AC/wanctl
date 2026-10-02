# S24: HTTP controller disconnect cancellation

The relay treated cancellation of an idle controller `/h/down` request as an empty poll and left the session queues open, so the agent's desktop watcher never received EOF. `httpconn.Close()` cancels that poll before posting `/h/close`; the CLI can exit after its read wakes but before that asynchronous POST completes, while an abruptly terminated controller never posts it at all. The fix ends the relay session immediately when an empty, unnumbered controller poll is cancelled, preserving data-bearing response retries, numbered prefetch cancellation and agent-side reply draining.

Changed files:

- `internal/relay/http.go`: the only production change, 13 added lines in `handleHDown`; uses the existing session teardown to wake the agent and close any WS bridge.
- `internal/agent/desktop_transport_test.go`: real loopback relay, HTTP/WS carriers, mutual TLS and desktop dispatch; only the runner is fake and blocks on `ctx.Done()`.
- `internal/relay/idle_disconnect_test.go`: guards existing data retransmission, prefetch cancellation and agent polling behavior.
- `last-message.md`: this report.

Tests:

- `TestDesktopDisconnectOverRelayCarriers`: all four controller/agent HTTP/WS combinations, each with normal connection close, CLI-style interrupt followed by process exit, and abrupt underlying TCP drop (12 cases). Each must cancel the runner within two seconds, leave its screenshot reference consumed, and reject redelivery over a fresh real session without invoking the runner again.
- The interrupt-exit case uses the production `wsconn.CloseOnCancel` path and waits for the controller read to return. A test-only barrier delays `/h/close` until simulated process exit drops the sockets, making the observed CLI exit race deterministic. The direct-drop case never calls TLS/carrier `Close()` or sends `/h/close`.
- `TestCancelledHTTPPollPreservesDataAndPrefetch`: cancellation of a data-bearing controller response retains the identical chunk/sequence for retry; cancelling numbered prefetch or an agent poll does not terminate the session.

Ablation was run with production code unchanged at `a232865` and the final desktop transport test added, before applying the relay fix:

```sh
go test ./internal/agent -run '^TestDesktopDisconnectOverRelayCarriers$' -count=1 -v
```

Relevant failure output:

```text
=== RUN   TestDesktopDisconnectOverRelayCarriers/agent=http/controller=http/interrupt-exit
    desktop_transport_test.go:94: runner context not cancelled within 2s after controller interrupt-exit (calls=1)
=== RUN   TestDesktopDisconnectOverRelayCarriers/agent=http/controller=http/drop
    desktop_transport_test.go:94: runner context not cancelled within 2s after controller drop (calls=1)
=== RUN   TestDesktopDisconnectOverRelayCarriers/agent=ws/controller=http/interrupt-exit
    desktop_transport_test.go:94: runner context not cancelled within 2s after controller interrupt-exit (calls=1)
=== RUN   TestDesktopDisconnectOverRelayCarriers/agent=ws/controller=http/drop
    desktop_transport_test.go:94: runner context not cancelled within 2s after controller drop (calls=1)
--- FAIL: TestDesktopDisconnectOverRelayCarriers (13.85s)
FAIL
FAIL    wanctl/internal/agent    14.364s
```

The other eight baseline cases passed: WS controllers already cancelled, and a synchronous HTTP close that actually delivered `/h/close` already worked. This distinguishes the CLI's early process exit from a fully delivered graceful close.

After the fix, the identical command passed all 12 cases; the largest observed runner cancellation latency in that loopback run was 0.180 ms. The same matrix passed three runs under `-race`. These are automated carrier/runner measurements, not Windows desktop or physical-input latency claims.

All required checks passed:

```sh
go build ./...
go vet ./...
go test ./...
GOOS=windows GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go vet ./...
GOOS=linux GOARCH=amd64 go build ./...
```

Additional checks passed:

```sh
go test ./internal/relay ./internal/httpconn -count=1
go test -race ./internal/agent -run '^TestDesktopDisconnectOverRelayCarriers$' -count=3 -v
go test -race ./internal/relay -run 'TestCancelledHTTPPollPreservesDataAndPrefetch|TestPushSurvivesTruncatedDownPoll|TestPushSurvivesDroppedDownPoll|TestGracefulClose' -count=1
go build -tags lark ./...
go vet -tags lark ./...
go test -tags lark ./...
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build ./...
```

Local logs: `build/s24-disconnect-ablation.log`, `build/s24-disconnect-fixed.log`, `build/s24-disconnect-race.log`, `build/s24-relay-http-tests.log`, `build/s24-relay-race.log`, `build/s24-go-test.log`, and `build/s24-go-test-lark.log`.

Ordinary synchronous `exec` also depends on peer EOF, so abrupt HTTP controller loss has the same underlying problem; its separate explicit cancel frame can already handle graceful interruptions. No exec, desktop protocol, approval, indicator, human-input or helper code was changed.

The repair is in the relay. For S24, rebuild/restart only the isolated test relay from this commit and repeat the Windows oracle check; the earlier `a232865` candidate archive remains unchanged. No real device or production relay was contacted, and nothing was pushed, tagged, released or written to a manifest.
