package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

func TestOpenCUInstallerDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, output, version string
		downloadErr, runErr   bool
		want                  string
	}{
		{"success", "ok", "0.4.0", false, false, ""},
		{"download", "", "", true, false, "download failed"},
		{"in use", "verbose\nOpenCU is in use (process IDs 123).\n", "", false, true, "OpenCU is in use"},
		{"concurrent", "another install is running", "", false, true, "another install"},
		{"exit", "", "", false, true, "exited unsuccessfully"},
		{"missing manifest", "ok", "", false, false, "no installed version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran, staged := false, false
			v, err := runOpenCUInstaller(t.Context(), func(context.Context) ([]byte, error) {
				if tc.downloadErr {
					return nil, errors.New("download failed")
				}
				return []byte("official script"), nil
			}, func(_ context.Context, script []byte) (string, error) {
				ran = true
				if string(script) != "official script" {
					t.Fatal("changed script")
				}
				if tc.runErr {
					return tc.output, errors.New("exit 1")
				}
				return tc.output, nil
			}, func() string { return tc.version }, func() { staged = true })
			if tc.want == "" {
				if err != nil || v != tc.version {
					t.Fatalf("%s %v", v, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v", err)
			}
			if tc.downloadErr && (ran || staged) {
				t.Fatal("ran after failed download")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := runOpenCUInstaller(ctx, func(context.Context) ([]byte, error) { return []byte("x"), nil }, func(context.Context, []byte) (string, error) { t.Fatal("ran cancelled install"); return "", nil }, func() string { return "" }, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestInstallerOutputBounds(t *testing.T) {
	var b installerOutput
	for i := 0; i < 10; i++ {
		b.Write([]byte(strings.Repeat("x", maxInstallerOutput)))
	}
	b.Write([]byte("\n\x1b[31mlast error\x1b[0m\x00\x1b\u202e\n"))
	if len(b.data) > maxInstallerOutput || cleanInstallerError(string(b.data)) != "last error" {
		t.Fatalf("%d %q", len(b.data), cleanInstallerError(string(b.data)))
	}
	if len([]rune(cleanInstallerError(strings.Repeat("界", 1000)))) != 400 {
		t.Fatal("uncapped error")
	}
}
func TestInstallHelperSessionLaunch(t *testing.T) {
	_, err := installOpenCUWith(t.Context(), func(context.Context) (*helperProcess, error) { return nil, errNoConsoleUser }, func() { t.Fatal("installing without user") })
	if !errors.Is(err, errNoConsoleUser) {
		t.Fatal(err)
	}
	staged := false
	v, err := installOpenCUWith(t.Context(), func(context.Context) (*helperProcess, error) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer outW.Close()
			defer inR.Close()
			_, raw, err := protocol.ReadFrame(inR)
			if err != nil {
				return
			}
			var job Job
			json.Unmarshal(raw, &job)
			if job.Action != "install_opencu" {
				t.Errorf("%+v", job)
			}
			for _, stage := range []string{"installing", "done"} {
				data, _ := json.Marshal(protocol.InstallProgress{Package: "opencu", Version: "0.4.0", Stage: stage})
				protocol.WriteMessage(outW, protocol.Message{Kind: protocol.KindInstallProgress, Data: data})
			}
		}()
		return &helperProcess{stdin: inW, stdout: outR, kill: func() error { return nil }, wait: func() error { <-done; return nil }}, nil
	}, func() { staged = true })
	if err != nil || v != "0.4.0" || !staged {
		t.Fatalf("%s %v %v", v, err, staged)
	}
}
func TestInstallPauseRefusesActiveDesktopAndClosesIdle(t *testing.T) {
	var s Service
	peer := &installIdlePeer{}
	s.cu = map[string]*openCUSession{"owner": {conn: peer}}
	s.busy.Lock()
	if _, ok := s.TryPauseForInstall(); ok {
		t.Fatal("queued active call")
	}
	s.busy.Unlock()
	resume, ok := s.TryPauseForInstall()
	if !ok {
		t.Fatal("desktop reservation failed")
	}
	s.CloseIdleOpenCU()
	if !peer.closed {
		t.Fatal("idle peer not closed")
	}
	if !s.Busy() {
		t.Fatal("new desktop call could enter during install")
	}
	resume()
	if s.Busy() {
		t.Fatal("lock leaked")
	}
}

type installIdlePeer struct{ closed bool }

func (p *installIdlePeer) Close() { p.closed = true }
func (p *installIdlePeer) Do(context.Context, protocol.DesktopRequest) (protocol.OpenCUResult, error) {
	return protocol.OpenCUResult{}, nil
}
