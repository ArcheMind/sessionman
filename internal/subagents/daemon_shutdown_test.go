package subagents

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArcheMind/agentx/internal/runtime"
)

type shutdownStdin struct {
	io.Writer
	closed chan struct{}
}

func (s shutdownStdin) Close() error {
	close(s.closed)
	return nil
}

func TestDaemonShutdownPreservesReplacement(t *testing.T) {
	dir := t.TempDir()
	meta := AgentMeta{ID: "replacement", OwnerPID: os.Getpid(), Socket: socketPath(dir, filepath.Base(filepath.Dir(dir)))}
	if err := WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	start := func() (*Daemon, <-chan error) {
		t.Helper()
		d := &Daemon{Dir: dir, Linger: time.Hour}
		result := make(chan error, 1)
		finished := make(chan struct{})
		go func() {
			result <- RunDaemon(d)
			close(finished)
		}()
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err := controlCall(meta.Socket, controlRequest{Op: "status"}); err == nil {
				break
			}
			select {
			case err := <-result:
				t.Fatalf("daemon exited before ready: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("daemon did not become ready")
			}
			time.Sleep(time.Millisecond)
		}
		t.Cleanup(func() {
			d.shutdown()
			d.mu.Lock()
			d.linger.Stop()
			d.mu.Unlock()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Error("daemon did not shut down")
			}
		})
		return d, result
	}
	old, oldResult := start()
	stdinClosed := make(chan struct{})
	procDone := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(procDone) }) }
	t.Cleanup(release)
	old.mu.Lock()
	old.proc = &nativeProc{
		stream: runtime.NewPipeStream(shutdownStdin{Writer: io.Discard, closed: stdinClosed}, strings.NewReader(""), nil, nil, nil),
		done:   procDone,
	}
	old.mu.Unlock()
	old.shutdown()
	select {
	case <-stdinClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("old daemon did not start waiting for its native process")
	}
	replacement, replacementResult := start()
	pidPath := filepath.Join(dir, "daemon.pid")
	pidFile, err := os.Stat(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	// Both daemons share a PID; only the PID file's identity distinguishes them.
	data, err := os.ReadFile(pidPath)
	if err != nil || string(data) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("replacement PID file: %q, %v", data, err)
	}
	release()
	select {
	case err := <-oldResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old daemon did not exit")
	}
	if _, err := controlCall(meta.Socket, controlRequest{Op: "status"}); err != nil {
		t.Errorf("old daemon removed replacement socket: %v", err)
	}
	if current, err := os.Stat(pidPath); err != nil || !os.SameFile(pidFile, current) {
		t.Errorf("old daemon removed or replaced replacement PID file: %v", err)
	}
	replacement.shutdown()
	select {
	case err := <-replacementResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement daemon did not exit")
	}
	for _, path := range []string{meta.Socket, pidPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("daemon left its own artifact %s: %v", path, err)
		}
	}
}
