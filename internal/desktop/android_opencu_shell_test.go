//go:build darwin || linux

package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"wanctl/internal/elevate"
	"wanctl/internal/opencu"
	"wanctl/internal/opencu/ocutest"
)

// posixChannel runs the generated placement and start commands with /bin/sh
// against a private directory standing in for /data/local/tmp. Only id, stat,
// sha256sum and app_process are substituted; no device, root identity or the
// real /data/local/tmp is involved.
type posixChannel struct {
	root, bin string
	// owner is the uid stat reports for files this test owns, standing in for
	// the shell user (2000) or for another user.
	owner  string
	before func(command string)
}

func (c *posixChannel) Kind() elevate.Kind { return elevate.KindADB }
func (c *posixChannel) Probe(context.Context) elevate.Status {
	return elevate.Status{Available: true, Link: elevate.LinkConnected}
}
func (c *posixChannel) Run(context.Context, string, string, io.Writer) (int, error) {
	return -1, fmt.Errorf("unexpected Run: engine must use dedicated streams")
}
func (c *posixChannel) Close() error { return nil }
func (c *posixChannel) Stream(ctx context.Context, command, cwd string) (elevate.Stream, error) {
	command = strings.ReplaceAll(command, "/data/local/tmp", c.root)
	if c.before != nil {
		c.before(command)
	}
	// Toybox's `stat -c '%u %A' FILE` and `sha256sum FILE`, as the device has them.
	stat, sum := `command stat -c '%u %A' "$3"`, ""
	if runtime.GOOS == "darwin" {
		stat, sum = `/usr/bin/stat -f '%u %Sp' "$3"`, `sha256sum() { /usr/bin/shasum -a 256 "$@"; }; `
	}
	prelude := `id() { printf '2000\n'; }; ` + sum +
		`stat() { o=$(` + stat + `) || return 1; set -- $o; [ "$1" = ` + strconv.Itoa(os.Getuid()) + ` ] && set -- ` + c.owner + ` "$2"; printf '%s %s\n' "$1" "$2"; }; `
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", prelude+command)
	cmd.Env = append(os.Environ(), "PATH="+c.bin+":/usr/bin:/bin")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, ow, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderr, ew, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = ow, ew
	err = cmd.Start()
	ow.Close()
	ew.Close()
	if err != nil {
		in.Close()
		out.Close()
		stderr.Close()
		return nil, err
	}
	s := &posixStream{cmd: cmd, in: in, out: out, stderr: stderr, done: make(chan struct{})}
	go func() {
		s.err = cmd.Wait()
		if ee, ok := s.err.(*exec.ExitError); ok {
			s.code, s.err = ee.ExitCode(), nil
		}
		close(s.done)
	}()
	return s, nil
}

type posixStream struct {
	cmd         *exec.Cmd
	in          io.WriteCloser
	out, stderr io.ReadCloser
	done        chan struct{}
	err         error
	code        int
	once        sync.Once
}

func (s *posixStream) Stdin() io.WriteCloser { return s.in }
func (s *posixStream) Stdout() io.ReadCloser { return s.out }
func (s *posixStream) Stderr() io.ReadCloser { return s.stderr }
func (s *posixStream) Wait() (int, error)    { <-s.done; return s.code, s.err }
func (s *posixStream) Close() error {
	s.once.Do(func() {
		s.in.Close()
		s.out.Close()
		s.stderr.Close()
		s.cmd.Process.Kill()
		<-s.done
	})
	return nil
}

const posixGenuineJar = "genuine release jar"

// posixHost returns an engine host whose cache already holds the genuine jar
// (downloaded from a local release), the cached jar's path, the device jar's
// path and the file the app_process stand-in records the loaded bytes in.
func posixHost(t *testing.T, binary string) (host *AndroidOpenCU, ch *posixChannel, cacheJar, deviceJar, loaded string) {
	t.Helper()
	files, device, bin := t.TempDir(), t.TempDir(), t.TempDir()
	body := []byte(posixGenuineJar)
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	m := opencu.AndroidManifest{Name: "opencu-android", Version: "1.2.3", File: "opencu-android.jar", Size: int64(len(body)), SHA256: hash}
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			json.NewEncoder(w).Encode(m)
		} else {
			w.Write(body)
		}
	}))
	t.Cleanup(release.Close)
	cache := opencu.NewAndroidCache(files, release.URL)
	e, done, err := cache.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done()
	loaded = filepath.Join(device, "loaded-by-app-process")
	script := "#!/bin/sh\ncat \"$CLASSPATH\" > " + elevate.QuotePOSIX(loaded) + "\nexec " + elevate.QuotePOSIX(binary) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "app_process"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ch = &posixChannel{root: device, bin: bin, owner: "2000"}
	host = &AndroidOpenCU{Cache: cache, Elevator: elevate.NewManager(true, "", ch)}
	return host, ch, e.Path, filepath.Join(device, "opencu-android-"+hash[:16]+".jar"), loaded
}

// What reaches the device is what the cache verified, even if the cached file
// is replaced after acquisition by a link to some other private file.
func TestAndroidPlacementUploadsTheVerifiedBytes(t *testing.T) {
	host, _, cacheJar, deviceJar, loaded := posixHost(t, ocutest.Build(t))
	private := filepath.Join(t.TempDir(), "private-data")
	payload := strings.Repeat("SYNTHETIC_PRIVATE_CONTENT", 100)
	if err := os.WriteFile(private, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	host.OnVersion = func(string) {
		if err := os.Remove(cacheJar); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(private, cacheJar); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := host.StartOpenCU(ctx)
	if err == nil {
		c.Close()
	}
	b, _ := os.ReadFile(deviceJar)
	if strings.Contains(string(b), "SYNTHETIC_PRIVATE_CONTENT") {
		t.Fatalf("%d bytes of a replaced cache file reached the device jar", len(b))
	}
	if err != nil || string(b) != posixGenuineJar {
		t.Fatalf("device jar %q, start error %v", b, err)
	}
	if got, _ := os.ReadFile(loaded); string(got) != posixGenuineJar {
		t.Fatalf("engine loaded %q", got)
	}
}

// A cache path replaced by a FIFO after acquisition is never opened, so it
// cannot hold up startup or its cancellation.
func TestAndroidStartIsNotBlockedByAReplacedCacheFile(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "completes", true: "cancelled"}[cancelled], func(t *testing.T) {
			host, _, cacheJar, deviceJar, _ := posixHost(t, ocutest.Build(t))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			host.OnVersion = func(string) {
				os.Remove(cacheJar)
				if err := syscall.Mkfifo(cacheJar, 0o600); err != nil {
					t.Error(err)
				}
				if cancelled {
					cancel()
				}
			}
			type result struct {
				c   OpenCUConnection
				err error
			}
			done := make(chan result, 1)
			go func() { c, err := host.StartOpenCU(ctx); done <- result{c, err} }()
			select {
			case r := <-done:
				if r.err == nil {
					r.c.Close()
				}
				if cancelled && r.err == nil {
					t.Fatal("cancelled start succeeded")
				}
				if !cancelled {
					if b, _ := os.ReadFile(deviceJar); r.err != nil || string(b) != posixGenuineJar {
						t.Fatalf("device jar %q, start error %v", b, r.err)
					}
				}
			case <-time.After(10 * time.Second):
				// Supply a writer so a blocked open returns and nothing leaks.
				if f, err := os.OpenFile(cacheJar, os.O_RDWR, 0); err == nil {
					f.Close()
				}
				if r := <-done; r.err == nil {
					r.c.Close()
				}
				t.Fatal("a FIFO at the cache path blocked startup")
			}
		})
	}
}

// A symlink already at the device path is refused, not followed.
func TestAndroidPlacementRefusesASymlinkAtTheDevicePath(t *testing.T) {
	host, _, _, deviceJar, loaded := posixHost(t, "unused")
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, deviceJar); err != nil {
		t.Fatal(err)
	}
	if _, err := host.StartOpenCU(context.Background()); err == nil {
		t.Fatal("symlink accepted")
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "unchanged" {
		t.Fatalf("symlink target changed: %q %v", b, err)
	}
	if _, err := os.Stat(loaded); !os.IsNotExist(err) {
		t.Fatal("engine started despite the symlink")
	}
}

// A device jar with the right hash but a looser mode is not reused: it is
// replaced by a fresh copy with mode 0644 before the engine starts from it.
func TestAndroidPlacementReplacesADeviceJarWithALooseMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o666, 0o664, 0o755} {
		t.Run(mode.String(), func(t *testing.T) {
			host, _, _, deviceJar, loaded := posixHost(t, ocutest.Build(t))
			if err := os.WriteFile(deviceJar, []byte(posixGenuineJar), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(deviceJar, mode); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c, err := host.StartOpenCU(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
			st, err := os.Lstat(deviceJar)
			if err != nil || st.Mode() != 0o644 {
				t.Fatalf("device jar kept mode %v (%v)", st.Mode(), err)
			}
			if b, _ := os.ReadFile(loaded); string(b) != posixGenuineJar {
				t.Fatalf("engine loaded %q", b)
			}
		})
	}
}

// A jar that is not owned by the shell user is never started, even with the
// right hash: placement cannot produce an acceptable copy, so startup fails.
func TestAndroidStartRefusesAJarNotOwnedByTheShellUser(t *testing.T) {
	host, ch, _, _, loaded := posixHost(t, ocutest.Build(t))
	ch.owner = "1000"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if c, err := host.StartOpenCU(ctx); err == nil {
		c.Close()
		t.Fatal("started a jar owned by another user")
	}
	if _, err := os.Stat(loaded); !os.IsNotExist(err) {
		t.Fatal("app_process ran a jar owned by another user")
	}
}

// The launch command repeats the owner and mode check: a jar whose mode was
// loosened, or that was replaced by a symlink, after verification is not run.
func TestAndroidLaunchRefusesAJarChangedAfterVerification(t *testing.T) {
	for _, change := range []string{"mode", "symlink"} {
		t.Run(change, func(t *testing.T) {
			host, ch, _, deviceJar, loaded := posixHost(t, ocutest.Build(t))
			ch.before = func(command string) {
				if !strings.Contains(command, "CLASSPATH=") {
					return
				}
				if change == "mode" {
					if err := os.Chmod(deviceJar, 0o666); err != nil {
						t.Error(err)
					}
					return
				}
				other := filepath.Join(t.TempDir(), "other.jar")
				if err := os.WriteFile(other, []byte(posixGenuineJar), 0o644); err != nil {
					t.Error(err)
				}
				os.Remove(deviceJar)
				if err := os.Symlink(other, deviceJar); err != nil {
					t.Error(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if c, err := host.StartOpenCU(ctx); err == nil {
				c.Close()
				t.Fatal("launched a jar changed after verification")
			}
			if _, err := os.Stat(loaded); !os.IsNotExist(err) {
				t.Fatal("app_process ran a jar changed after verification")
			}
		})
	}
}
