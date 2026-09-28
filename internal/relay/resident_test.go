package relay

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/httpconn"
	"wanctl/internal/sessionauth"
)

const mebibyte = 1 << 20

// residentOf reports what one direction holds.
func residentOf(q *sideQueue) int64 {
	q.ackMu.Lock()
	defer q.ackMu.Unlock()
	return q.resident
}

func setDirectionLimit(q *sideQueue, n int64) {
	q.ackMu.Lock()
	q.limit = n
	q.ackMu.Unlock()
}

// watchedBody is a request body that counts what the relay has read of it.
type watchedBody struct {
	r    io.Reader
	read atomic.Int64
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// upload posts one /h/up straight into the handler from a goroutine, since a
// write may have to wait for room, and delivers its status when it answers.
func upload(h http.Handler, token, sid, role string, seq uint64, body io.Reader, size int64) <-chan int {
	u := "/h/up?session=" + sid + "&role=" + role
	if seq != 0 {
		u += "&" + httpconn.UpSeqParam + "=" + strconv.FormatUint(seq, 10)
	}
	req := httptest.NewRequest(http.MethodPost, u, body)
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+token)
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	return done
}

func uploadBytes(h http.Handler, sid string, seq uint64, b []byte) <-chan int {
	return upload(h, "tok-alice", sid, "client", seq, bytes.NewReader(b), int64(len(b)))
}

func mustAnswer(t *testing.T, done <-chan int, want int, what string) {
	t.Helper()
	select {
	case code := <-done:
		if code != want {
			t.Fatalf("%s answered %d, want %d", what, code, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never answered", what)
	}
}

func mustWait(t *testing.T, done <-chan int, what string) {
	t.Helper()
	select {
	case code := <-done:
		t.Fatalf("%s answered %d while the relay had no room for it; it should have waited", what, code)
	case <-time.After(200 * time.Millisecond):
	}
}

// marked is a write whose bytes say which write they came from.
func marked(seq uint64, n int) []byte { return bytes.Repeat([]byte{byte(seq)}, n) }

// readAcked reads a direction through acknowledged polls until it has n bytes,
// checking after each one that the direction stayed within limit. It returns
// the sequence of the last chunk, which it leaves unacknowledged.
func readAcked(t *testing.T, r *Relay, sid, role string, q *sideQueue, n int, limit int64) ([]byte, uint64) {
	t.Helper()
	var got []byte
	var ack uint64
	for len(got) < n {
		rec := downPoll(t, r, sid, role, ack)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll after %d of %d bytes = %d", len(got), n, rec.Code)
		}
		got = append(got, rec.Body.Bytes()...)
		seq, err := strconv.ParseUint(rec.Header().Get(httpconn.DownSeqHeader), 10, 64)
		if err != nil {
			t.Fatalf("poll carried no sequence: %v", err)
		}
		ack = seq
		if held := residentOf(q); held > limit {
			t.Fatalf("the direction held %d bytes, over its %d-byte budget", held, limit)
		}
	}
	return got, ack
}

// acknowledge does what the next poll would do first, without waiting on an
// empty queue the way a poll does.
func acknowledge(q *sideQueue, ack uint64) {
	q.ackMu.Lock()
	q.dropAcked(ack)
	q.ackMu.Unlock()
}

// Writes that wait for an earlier one are held on the relay, but only within
// the direction's budget: past it a write waits unread, room is always left
// for the write the others wait on, and a write further ahead than the window
// is sent back unread to be tried again.
func TestResidentBytesBehindAGapStayWithinTheBudget(t *testing.T) {
	r, s, sid := tunnelSession(t)
	const limit = 8 * mebibyte // a window of 6 writes
	setDirectionLimit(s.toAgent, limit)
	h := r.Handler()
	var want []byte
	for seq := uint64(1); seq <= 10; seq++ {
		want = append(want, marked(seq, mebibyte)...)
	}

	// Nobody is reading yet. Writes 1 to 3 are queued; the writer has not sent
	// write 4, and writes 5 to 8 wait for it here.
	for _, seq := range []uint64{1, 2, 3, 5, 6, 7, 8} {
		mustAnswer(t, uploadBytes(h, sid, seq, marked(seq, mebibyte)), http.StatusOK, fmt.Sprintf("write %d", seq))
	}
	if got := residentOf(s.toAgent); got != 7*mebibyte {
		t.Fatalf("the direction holds %d bytes, want the 7 MiB written so far", got)
	}

	// Write 9 would leave no room for write 4, so it waits, and none of its
	// bytes are read while it does.
	body9 := &watchedBody{r: bytes.NewReader(marked(9, mebibyte))}
	done9 := upload(h, "tok-alice", sid, "client", 9, body9, mebibyte)
	mustWait(t, done9, "write 9")
	// Write 11 is past the window: sent back at once, unread.
	body11 := &watchedBody{r: bytes.NewReader(marked(11, mebibyte))}
	mustAnswer(t, upload(h, "tok-alice", sid, "client", 11, body11, mebibyte), http.StatusServiceUnavailable, "write 11")
	for seq, body := range map[int]*watchedBody{9: body9, 11: body11} {
		if n := body.read.Load(); n != 0 {
			t.Fatalf("the relay read %d bytes of write %d, which it had no room for", n, seq)
		}
	}
	if got := residentOf(s.toAgent); got > limit {
		t.Fatalf("the direction holds %d bytes, over its %d-byte budget", got, limit)
	}

	// Write 4 still fits. Once the reader reads, everything comes out in
	// order, write 9 included, and write 10 after it.
	mustAnswer(t, uploadBytes(h, sid, 4, marked(4, mebibyte)), http.StatusOK, "write 4")
	done10 := uploadBytes(h, sid, 10, marked(10, mebibyte))
	got, last := readAcked(t, r, sid, "agent", s.toAgent, 10*mebibyte, limit)
	if !bytes.Equal(got, want) {
		t.Fatalf("the writes did not come out whole and in order (%d bytes read)", len(got))
	}
	mustAnswer(t, done9, http.StatusOK, "write 9")
	mustAnswer(t, done10, http.StatusOK, "write 10")
	acknowledge(s.toAgent, last)
	if got := residentOf(s.toAgent); got != 0 {
		t.Fatalf("the direction still holds %d bytes after its reader acknowledged everything", got)
	}
}

// dialRole opens one end of session sid through the real client, the way
// /h/dial and /h/poll leave it.
func dialRole(t *testing.T, url, sid, role string, hc *http.Client) net.Conn {
	t.Helper()
	nc, err := httpconn.DialWith(t.Context(), url, sid, role, "tok-alice", hc)
	if err != nil {
		t.Fatal(err)
	}
	httpconn.MarkOrdered(nc)
	httpconn.MarkWindow(nc)
	return nc
}

func dialPair(t *testing.T, url, sid string, hc *http.Client) (client, agent net.Conn) {
	t.Helper()
	return dialRole(t, url, sid, "client", hc), dialRole(t, url, sid, "agent", hc)
}

// A reader that does not read cannot make the relay queue more than the
// direction's budget: the writer is held back instead, and goes on as soon as
// the reader catches up, with every byte intact.
func TestResidentBytesQueuedForAnIdleReader(t *testing.T) {
	r, s, sid := tunnelSession(t)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	client, agent := dialPair(t, srv.URL, sid, &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}})
	defer agent.Close()

	payload := make([]byte, 48*mebibyte)
	rand.Read(payload)
	wrote := make(chan error, 1)
	go func() {
		_, err := client.Write(payload)
		if err == nil {
			err = client.Close()
		}
		wrote <- err
	}()

	// Watch the direction fill while nobody reads it.
	var peak int64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && peak < maxResidentPerDirection-maxUploadBytes {
		peak = max(peak, residentOf(s.toAgent))
		time.Sleep(time.Millisecond)
	}
	settle := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(settle) {
		peak = max(peak, residentOf(s.toAgent))
		time.Sleep(time.Millisecond)
	}
	if peak > maxResidentPerDirection {
		t.Fatalf("an unread direction held %d bytes, over its %d-byte budget", peak, maxResidentPerDirection)
	}
	if peak < maxResidentPerDirection-maxUploadBytes {
		t.Fatalf("an unread direction only ever held %d bytes; the writer stopped short of the budget", peak)
	}
	select {
	case err := <-wrote:
		t.Fatalf("the writer finished (%v) with nobody reading 48 MiB", err)
	default:
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(agent, got); err != nil {
		t.Fatalf("reading after the writer was held back: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(payload) {
		t.Fatal("the stream changed on its way through a full direction")
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the writer never finished after its reader caught up")
	}
}

// windowTake is a numbered poll that takes a chunk of up to max bytes and
// acknowledges nothing past ack.
func windowTake(t *testing.T, r *Relay, sid string, ack, want uint64, max int) *httptest.ResponseRecorder {
	t.Helper()
	u := fmt.Sprintf("/h/down?session=%s&role=agent&ack=%d&want=%d&max=%d", sid, ack, want, max)
	req := httptest.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec
}

// Bytes a reader has been handed but has not acknowledged are still the
// relay's to hold, so a reader that takes chunks and never acknowledges them
// holds the writer back as surely as one that never reads.
func TestResidentBytesCountChunksAReaderNeverAcknowledged(t *testing.T) {
	r, s, sid := tunnelSession(t)
	const limit = 8 * mebibyte
	setDirectionLimit(s.toAgent, limit)
	h := r.Handler()
	for seq := uint64(1); seq <= 8; seq++ {
		mustAnswer(t, uploadBytes(h, sid, seq, marked(seq, mebibyte)), http.StatusOK, fmt.Sprintf("write %d", seq))
	}

	// The reader takes all of it in four chunks and acknowledges none.
	for want := uint64(1); want <= 4; want++ {
		if rec := windowTake(t, r, sid, 0, want, 2*mebibyte); rec.Code != http.StatusOK || rec.Body.Len() != 2*mebibyte {
			t.Fatalf("take %d = %d with %d bytes", want, rec.Code, rec.Body.Len())
		}
	}
	if queued := len(s.toAgent.ch); queued != 0 {
		t.Fatalf("%d chunks still queued; the reader should have taken everything", queued)
	}
	if got := residentOf(s.toAgent); got != limit {
		t.Fatalf("the direction holds %d bytes with 8 MiB handed out unacknowledged, want %d", got, limit)
	}

	body9 := &watchedBody{r: bytes.NewReader(marked(9, mebibyte))}
	done9 := upload(h, "tok-alice", sid, "client", 9, body9, mebibyte)
	mustWait(t, done9, "a write while the reader holds everything unacknowledged")
	// Asking for the same chunks again takes nothing new.
	if rec := windowTake(t, r, sid, 0, 1, 2*mebibyte); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes()[:1], []byte{1}) {
		t.Fatalf("re-poll = %d", rec.Code)
	}
	mustWait(t, done9, "a write while the reader re-reads what it holds")
	if n := body9.read.Load(); n != 0 {
		t.Fatalf("the relay read %d bytes of a write it had no room for", n)
	}

	// Acknowledging is what makes room.
	next := windowTake(t, r, sid, 4, 5, 2*mebibyte)
	mustAnswer(t, done9, http.StatusOK, "the write after the reader acknowledged")
	if next.Code != http.StatusOK || !bytes.Equal(next.Body.Bytes(), marked(9, mebibyte)) {
		t.Fatalf("poll after the acknowledgement = %d with %d bytes, want write 9", next.Code, next.Body.Len())
	}
}

// tenantSession registers a session for a namespace of its own on r, as
// /h/dial does.
func tenantSession(t *testing.T, r *Relay, sid, ns string) *httpSession {
	t.Helper()
	auth := sessionauth.Open{Session: sid, Device: "dev", CallerNamespace: ns, OwnerNamespace: ns}
	s := r.newHTTPSession(sid, auth, delegation.Access{Namespace: ns}, "tok-"+ns)
	t.Cleanup(func() { r.closeHTTPSession(sid, s) })
	return s
}

func push(t *testing.T, q *sideQueue, n int) <-chan bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- q.push(make([]byte, n)) }()
	return done
}

func mustPush(t *testing.T, done <-chan bool, what string) {
	t.Helper()
	select {
	case ok := <-done:
		if !ok {
			t.Fatalf("%s was refused", what)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never went through", what)
	}
}

func mustHold(t *testing.T, done <-chan bool, what string) {
	t.Helper()
	select {
	case ok := <-done:
		t.Fatalf("%s went through (%v) with the relay out of room", what, ok)
	case <-time.After(200 * time.Millisecond):
	}
}

// Every session on a relay draws on one budget, so many sessions that each
// stay inside their own cannot add up to more than it.
func TestResidentBytesRelayWideBudget(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice,tok-bob:bob"))
	r.resident.limit = 4 * mebibyte
	a := tenantSession(t, r, "sess-a", "alice")
	b := tenantSession(t, r, "sess-b", "bob")

	for i := range 3 {
		mustPush(t, push(t, a.toAgent, mebibyte), fmt.Sprintf("alice's write %d", i))
	}
	mustPush(t, push(t, b.toAgent, mebibyte), "bob's first write")
	held := push(t, b.toAgent, mebibyte)
	mustHold(t, held, "bob's second write")
	if got := r.resident.resident(); got > 4*mebibyte {
		t.Fatalf("the relay holds %d bytes, over its %d-byte budget", got, 4*mebibyte)
	}

	// Any reader making room lets it through, whoever's session it is.
	if data, _, _ := a.toAgent.pollDrain(t.Context(), time.Second); len(data) == 0 {
		t.Fatal("alice's reader got nothing")
	}
	mustPush(t, held, "bob's second write once alice's reader made room")
}

// One namespace's sessions together get a share of the relay's budget, so an
// account that never reads what it queues holds back its own writes and no
// one else's.
func TestResidentBytesNamespaceShare(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice,tok-bob:bob"))
	r.resident.nsLimit = 3 * mebibyte
	a1 := tenantSession(t, r, "sess-a1", "alice")
	a2 := tenantSession(t, r, "sess-a2", "alice")
	b := tenantSession(t, r, "sess-b", "bob")

	mustPush(t, push(t, a1.toAgent, mebibyte), "alice's first write")
	mustPush(t, push(t, a2.toClient, 2*mebibyte), "alice's second write, on another session")
	held := push(t, a1.toAgent, mebibyte)
	mustHold(t, held, "alice's third write")
	for i := range 3 {
		mustPush(t, push(t, b.toAgent, mebibyte), fmt.Sprintf("bob's write %d", i))
	}

	if data, _, _ := a2.toClient.pollDrain(t.Context(), time.Second); len(data) == 0 {
		t.Fatal("alice's reader got nothing")
	}
	mustPush(t, held, "alice's third write once her own reader made room")
}

// A write that waits for room when its session ends is refused, not left
// waiting, and whatever the session held goes back to the relay however it
// ended: nothing that left the registry stays counted.
func TestResidentBytesReleasedWhenSessionsEnd(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	h := r.Handler()
	// fill leaves a session holding bytes in every state there is: queued,
	// handed out and not acknowledged, split at the drain cap, and waiting in
	// seqHeld for a write that never comes.
	const part = 3 * mebibyte / 4 // three of them straddle the drain cap
	fill := func(t *testing.T, sid string) *httpSession {
		s := tenantSession(t, r, sid, "alice")
		for seq := uint64(1); seq <= 3; seq++ {
			mustAnswer(t, uploadBytes(h, sid, seq, marked(seq, part)), http.StatusOK, "write")
		}
		mustAnswer(t, uploadBytes(h, sid, 5, marked(5, mebibyte)), http.StatusOK, "write after a gap")
		if rec := downPoll(t, r, sid, "agent", 0); rec.Code != http.StatusOK {
			t.Fatalf("take = %d", rec.Code)
		}
		mustPush(t, push(t, s.toClient, mebibyte), "agent's write")
		if residentOf(s.toAgent) == 0 || residentOf(s.toClient) == 0 {
			t.Fatal("the session holds nothing to give back")
		}
		return s
	}
	ends := map[string]func(t *testing.T, sid string, s *httpSession){
		"closed by the relay": func(t *testing.T, sid string, s *httpSession) { r.closeHTTPSession(sid, s) },
		"reaped as idle": func(t *testing.T, sid string, s *httpSession) {
			r.reapHTTP(time.Now().Add(unackedRetention + time.Minute))
		},
		"credential lease ended": func(t *testing.T, sid string, s *httpSession) { s.lease.close() },
		"closed gracefully and drained": func(t *testing.T, sid string, s *httpSession) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/h/close?session="+sid, nil)
			req.Header.Set("Authorization", "Bearer tok-alice")
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("close = %d", rec.Code)
			}
			// Everything before the gap comes out; the write after it never can.
			_, lastA := readAcked(t, r, sid, "agent", s.toAgent, 3*part, maxResidentPerDirection)
			_, lastC := readAcked(t, r, sid, "client", s.toClient, mebibyte, maxResidentPerDirection)
			for _, end := range []*httptest.ResponseRecorder{downPoll(t, r, sid, "agent", lastA), downPoll(t, r, sid, "client", lastC)} {
				if end.Code != http.StatusGone {
					t.Fatalf("poll past the end = %d, want 410", end.Code)
				}
			}
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			sid := "sess-" + strings.ReplaceAll(name, " ", "-")
			s := fill(t, sid)
			setDirectionLimit(s.toClient, 2*mebibyte)
			waiting := push(t, s.toClient, 2*mebibyte)
			mustHold(t, waiting, "a write with its direction full")
			end(t, sid, s)
			if r.session(sid) != nil {
				t.Fatal("the session is still registered")
			}
			select {
			case ok := <-waiting:
				if ok {
					t.Fatal("a write that was waiting for room was taken by a session that ended")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a write waiting for room was left waiting after its session ended")
			}
			if got := r.resident.resident(); got != 0 {
				t.Fatalf("the relay still counts %d bytes after the session ended", got)
			}
			r.resident.mu.Lock()
			shares := len(r.resident.ns)
			r.resident.mu.Unlock()
			if shares != 0 {
				t.Fatalf("%d namespace shares outlived their sessions", shares)
			}
		})
	}
}

// lostOnce fails the first attempt of one upload before it reaches the relay,
// the way a connection that drops mid-request does.
type lostOnce struct {
	base http.RoundTripper
	seq  string
	mu   sync.Mutex
	lost bool
}

func (l *lostOnce) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/h/up" && req.URL.Query().Get(httpconn.UpSeqParam) == l.seq {
		l.mu.Lock()
		first := !l.lost
		l.lost = true
		l.mu.Unlock()
		if first {
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, errors.New("carrier: connection reset before the request went out")
		}
	}
	return l.base.RoundTrip(req)
}

// While one upload is being retried, a fast writer goes on posting the ones
// after it, far past the window the relay holds. Those are sent back to be
// tried again rather than refused outright, so the transfer survives it.
func TestUploadRetriedBehindAFastWriter(t *testing.T) {
	r, _, sid := tunnelSession(t)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	tr := &http.Transport{MaxIdleConnsPerHost: 16}
	client := dialRole(t, srv.URL, sid, "client", &http.Client{Transport: &lostOnce{base: tr, seq: "3"}})
	agent := dialRole(t, srv.URL, sid, "agent", &http.Client{Transport: tr})
	defer agent.Close()

	payload := make([]byte, 64*mebibyte)
	rand.Read(payload)
	wrote := make(chan error, 1)
	go func() {
		_, err := client.Write(payload)
		if err == nil {
			err = client.Close()
		}
		wrote <- err
	}()
	got := make([]byte, len(payload))
	read := make(chan error, 1)
	go func() { _, err := io.ReadFull(agent, got); read <- err }()
	for range 2 {
		select {
		case err := <-wrote:
			if err != nil {
				t.Fatalf("writer: %v", err)
			}
		case err := <-read:
			if err != nil {
				t.Fatalf("reader: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the transfer stalled after one upload was lost")
		}
	}
	if sha256.Sum256(got) != sha256.Sum256(payload) {
		t.Fatal("the stream changed after one upload was lost and retried")
	}
}

// A writer that knows the relay orders writes sends its last bytes with the
// close: up to one full write of them, which is more than a control request
// is allowed to carry.
func TestCloseCarriesAWritersLastBytes(t *testing.T) {
	for _, size := range []int{100 << 10, 900 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			r, _, sid := tunnelSession(t)
			srv := httptest.NewServer(r.Handler())
			defer srv.Close()
			client, agent := dialPair(t, srv.URL, sid, &http.Client{Transport: &http.Transport{}})
			defer agent.Close()
			payload := marked(7, size)
			if _, err := client.Write(payload); err != nil {
				t.Fatal(err)
			}
			// Straight away, so the bytes are still pending and go with the close.
			if err := client.Close(); err != nil {
				t.Fatalf("close with %d bytes still to send: %v", size, err)
			}
			got := make(chan []byte, 1)
			go func() { b, _ := io.ReadAll(agent); got <- b }()
			select {
			case b := <-got:
				if !bytes.Equal(b, payload) {
					t.Fatalf("the far side read %d bytes, want the %d sent before the close", len(b), size)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the far side never reached the end of the stream")
			}
		})
	}
}

// A transfer has to keep moving when the budget, not the link, is the limit:
// every write and every poll then waits on room at some point, in every order
// the two sides can arrive in. The other direction carries a little traffic at
// the same time, as the far side's answers do. The budget here is the least
// that keeps a writer moving (see maxResidentPerDirection).
func TestTransferCompletesWhileTheBudgetBinds(t *testing.T) {
	r, s, sid := tunnelSession(t)
	const limit = 9 * mebibyte
	setDirectionLimit(s.toAgent, limit)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	client, agent := dialPair(t, srv.URL, sid, &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}})
	defer client.Close()
	defer agent.Close()
	bulk := make([]byte, 48*mebibyte+12345)
	answers := make([]byte, 2*mebibyte+777)
	rand.Read(bulk)
	rand.Read(answers)
	errs := make(chan error, 4)
	send := func(c net.Conn, p []byte) {
		for len(p) > 0 { // uneven writes, so batches are not all full
			n := min(len(p), 300<<10+len(p)%7919)
			if _, err := c.Write(p[:n]); err != nil {
				errs <- err
				return
			}
			p = p[n:]
		}
		errs <- nil
	}
	check := func(c net.Conn, want []byte) {
		got := make([]byte, 0, len(want))
		buf := make([]byte, 64<<10)
		for len(got) < len(want) { // uneven reads, so polls come at odd times
			n, err := c.Read(buf[:min(len(buf), 1+len(got)%len(buf), len(want)-len(got))])
			if err != nil {
				errs <- err
				return
			}
			got = append(got, buf[:n]...)
		}
		if sha256.Sum256(got) != sha256.Sum256(want) {
			errs <- errors.New("SHA-256 mismatch")
			return
		}
		errs <- nil
	}
	var peak atomic.Int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if held := residentOf(s.toAgent); held > peak.Load() {
				peak.Store(held)
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()
	defer close(stop)
	go send(client, bulk)
	go send(agent, answers)
	go check(agent, bulk)
	go check(client, answers)
	for range 4 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("a transfer bound by the budget stalled")
		}
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("the direction held %d bytes, over its %d-byte budget", got, limit)
	}
}
