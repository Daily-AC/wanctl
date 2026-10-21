package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	"wanctl/internal/opencu"
	"wanctl/internal/protocol"
)

// RunHelper carries private action data over pipes, never argv,
// environment, a temporary file, the event log or process error text.
func RunHelper(ctx context.Context, job Job) (protocol.DesktopResult, []byte, error) {
	return runPlatformHelper(ctx, job)
}

type helperProcess struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	wait   func() error
	kill   func() error
}

type helperStart func(context.Context) (*helperProcess, error)

func startInheritedHelper(context.Context) (*helperProcess, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, "__desktop")
	helperProcessAttrs(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	// Child errors are structured and sanitized. Never reflect stderr (which
	// might contain OS diagnostics that echo launch arguments) to a controller.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	return &helperProcess{stdin: stdin, stdout: stdout, wait: cmd.Wait, kill: cmd.Process.Kill}, nil
}

func exchangeHelper(ctx context.Context, job Job, start helperStart) (protocol.DesktopResult, []byte, error) {
	var res protocol.DesktopResult
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return res, nil, err
	}
	child, err := start(ctx)
	if err != nil {
		return res, nil, err
	}
	stdin, stdout := child.stdin, child.stdout
	defer stdout.Close()
	done := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			// EOF is a cooperative stop: the helper releases input before exiting.
			stdin.Close()
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				_ = child.kill()
			}
		case <-done:
		}
	}()
	raw, err := json.Marshal(job)
	if err == nil {
		err = protocol.WriteFrame(stdin, protocol.FrameJSON, raw)
	}
	var data []byte
	if err == nil {
		res, data, err = ReadResult(stdout)
	}
	if err != nil {
		stdin.Close()
		_ = child.kill()
	}
	waitErr := child.wait()
	close(done)
	<-watchDone
	stdin.Close()
	if err == nil {
		err = waitErr
	}
	return res, data, err
}

func WriteResult(w io.Writer, res protocol.DesktopResult, data []byte) error {
	if res.OpenCU != nil {
		raw, err := json.Marshal(res.OpenCU)
		if err != nil || len(raw) > opencu.MaxResult {
			return errors.New("OpenCU result exceeds 64 MiB; outcome unknown; observe again, never replay")
		}
		res.OpenCU = nil
		res.OpenCUBytes = len(raw)
		if err = protocol.WriteMessage(w, protocol.Message{Kind: protocol.KindDesktopResult, DesktopResult: &res}); err != nil {
			return err
		}
		for len(raw) > 0 {
			n := min(len(raw), 256<<10)
			if err = protocol.WriteFrame(w, protocol.FrameData, raw[:n]); err != nil {
				return err
			}
			raw = raw[n:]
		}
		return nil
	}
	res.ImageBytes = len(data)
	if len(data) > 0 {
		res.MIME = "image/jpeg"
	}
	if err := protocol.WriteMessage(w, protocol.Message{Kind: protocol.KindDesktopResult, DesktopResult: &res}); err != nil {
		return err
	}
	if len(data) > 0 {
		return protocol.WriteFrame(w, protocol.FrameData, data)
	}
	return nil
}
func ReadResult(r io.Reader) (protocol.DesktopResult, []byte, error) {
	m, err := protocol.ReadMessage(r)
	if err != nil {
		return protocol.DesktopResult{}, nil, err
	}
	return ReadResultBody(r, m)
}
func ReadResultBody(r io.Reader, m protocol.Message) (protocol.DesktopResult, []byte, error) {
	if m.Kind != protocol.KindDesktopResult || m.DesktopResult == nil {
		return protocol.DesktopResult{}, nil, errors.New("invalid desktop result header")
	}
	res := *m.DesktopResult
	if res.OpenCUBytes != 0 {
		if res.OpenCUBytes < 0 || res.OpenCUBytes > opencu.MaxResult || res.ImageBytes != 0 || res.OpenCU != nil {
			return res, nil, errors.New("invalid OpenCU result length")
		}
		raw := make([]byte, 0, res.OpenCUBytes)
		for len(raw) < res.OpenCUBytes {
			ft, b, err := protocol.ReadFrame(r)
			if err != nil {
				return res, nil, err
			}
			if ft != protocol.FrameData || len(b) == 0 || len(b) > 256<<10 || len(b) > res.OpenCUBytes-len(raw) {
				return res, nil, errors.New("invalid OpenCU result chunk")
			}
			raw = append(raw, b...)
		}
		if json.Unmarshal(raw, &res.OpenCU) != nil || res.OpenCU == nil {
			return res, nil, errors.New("invalid OpenCU result")
		}
		res.OpenCUBytes = 0
		return res, nil, nil
	}
	if res.ImageBytes < 0 || res.ImageBytes > protocol.MaxFrame {
		return res, nil, errors.New("invalid desktop image length")
	}
	if res.ImageBytes == 0 {
		return res, nil, nil
	}
	if res.MIME != "image/jpeg" || res.Snapshot == nil {
		return res, nil, errors.New("invalid desktop image metadata")
	}
	ft, data, err := protocol.ReadFrame(r)
	if err != nil {
		return res, nil, err
	}
	if ft != protocol.FrameData || len(data) != res.ImageBytes || len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return res, nil, errors.New("invalid desktop JPEG frame")
	}
	return res, data, nil
}

// HelperMain is reachable before relay/config initialization, in the selected
// user's desktop. Legacy jobs exit after one call; OpenCU jobs reuse a peer.
// Input EOF means the owner agent left.
func HelperMain(in io.Reader, out io.Writer) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ft, raw, err := protocol.ReadFrame(in)
	if err != nil || ft != protocol.FrameJSON {
		return 1
	}
	var job Job
	if json.Unmarshal(raw, &job) != nil {
		return 1
	}
	if job.Action == "install_opencu" {
		return installHelperMain(in, out)
	}
	if err = Validate(job.Action, &job.Request); err != nil {
		_ = WriteResult(out, protocol.DesktopResult{Status: "rejected", Error: err.Error(), FailedIndex: -1}, nil)
		return 0
	}
	if job.Request.IsOpenCU() {
		return openCUHelperMain(in, out, job)
	}
	go func() { var b [1]byte; _, _ = in.Read(b[:]); cancel() }()
	res, data := executeJob(ctx, job)
	if err = WriteResult(out, res, data); err != nil {
		return 1
	}
	return 0
}
