package relay

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func distServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	t.Setenv("WANCTL_DIST_DIR", signedDistWith(t, payload))
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func randomPayload(t *testing.T, n int) []byte {
	t.Helper()
	payload := make([]byte, n)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// A download is sent from the file, not from a copy of it in memory: what one
// request allocates must not grow with the artifact. (Release artifacts run to
// tens of MiB; a copy per request is what let concurrent downloads, or slow
// ones, run the relay out of memory.)
func TestDownloadAllocationDoesNotScaleWithTheArtifact(t *testing.T) {
	const size = 16 << 20
	srv := distServer(t, randomPayload(t, size))
	download := func() uint64 {
		t.Helper()
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		resp, err := srv.Client().Get(srv.URL + "/dl/wanctl-linux-amd64")
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || n != size {
			t.Fatalf("download: status %d, %d bytes, %v", resp.StatusCode, n, err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	least := download()
	for i := 0; i < 2; i++ {
		least = min(least, download())
	}
	if least > size/4 {
		t.Fatalf("one %d MiB download allocated %d MiB; it should stream from disk", size>>20, least>>20)
	}
	t.Logf("one %d MiB download allocated %d KiB", size>>20, least>>10)
}

// slowDownload starts a download on behalf of client (named the way the
// reverse proxy names it) and reads nothing past the response headers, the
// way a slow or deliberately stalled reader does. It returns the status and
// the connection, which stays open until the test ends.
func slowDownload(t *testing.T, srv *httptest.Server, client string) (int, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	// A small receive buffer keeps the kernel from swallowing the artifact on
	// the reader's behalf, so the relay really is left waiting to write.
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetReadBuffer(4 << 10)
	}
	fmt.Fprintf(conn, "GET /dl/wanctl-linux-amd64 HTTP/1.1\r\nHost: relay\r\nX-Real-IP: %s\r\n\r\n", client)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReaderSize(conn, 16), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Time{})
	return resp.StatusCode, conn
}

// Readers that stall mid-download hold a connection each, and nothing more:
// the relay's heap must not grow by an artifact per stalled reader.
func TestStalledDownloadsDoNotHoldTheArtifactInMemory(t *testing.T) {
	const size, readers = 8 << 20, 4
	srv := distServer(t, randomPayload(t, size))
	var before, during runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < readers; i++ {
		if status, _ := slowDownload(t, srv, fmt.Sprintf("198.51.100.%d", i+1)); status != http.StatusOK {
			t.Fatalf("stalled reader %d got %d", i+1, status)
		}
	}
	time.Sleep(200 * time.Millisecond) // let each handler fill the socket and block
	runtime.GC()
	runtime.ReadMemStats(&during)
	grew := int64(during.HeapAlloc) - int64(before.HeapAlloc)
	if grew > size {
		t.Fatalf("%d stalled readers grew the heap by %d MiB, more than one %d MiB artifact", readers, grew>>20, size>>20)
	}
	t.Logf("%d stalled readers of a %d MiB artifact grew the heap by %d KiB", readers, size>>20, grew>>10)
}

// Stalled readers from one address use up that address's share of downloads
// and nobody else's; closing them hands the share back.
func TestStalledReadersFromOneClientLeaveOthersTheirDownloads(t *testing.T) {
	srv := distServer(t, randomPayload(t, 8<<20))
	var stalled []net.Conn
	for i := 0; i < maxDownloadsPerClient; i++ {
		status, conn := slowDownload(t, srv, "198.51.100.7")
		if status != http.StatusOK {
			t.Fatalf("stalled reader %d got %d", i+1, status)
		}
		stalled = append(stalled, conn)
	}
	if status, _ := slowDownload(t, srv, "198.51.100.7"); status != http.StatusServiceUnavailable {
		t.Fatalf("one more download from the same client = %d, want 503", status)
	}
	fetch := func(client string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/dl/wanctl-linux-amd64", nil)
		req.Header.Set("X-Real-IP", client)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if status := fetch("198.51.100.8"); status != http.StatusOK {
		t.Fatalf("another client's download = %d, want 200", status)
	}
	for _, conn := range stalled {
		conn.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for fetch("198.51.100.7") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("closing the stalled downloads did not hand their slots back")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
