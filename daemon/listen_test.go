package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// socketDir returns a fresh directory for a test's Unix socket. Not
// t.TempDir: a socket's path is capped at 104 bytes on the BSDs and
// macOS, and a temp dir named for the test can run past it.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// A listen address that is an absolute path is a Unix socket: the
// daemon serves its control API there, the CLI reaches it there, and
// who may connect is decided by the file — the daemon makes it its own
// user's alone — and by whoever can reach the directory it is in.
func TestServeOnAUnixSocket(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	sock := filepath.Join(socketDir(t), "control.sock")

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- b.Serve(ctx, sock) }()
	waitForSocket(t, sock)

	info, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Type() != os.ModeSocket {
		t.Fatalf("%s is %v, want a socket", sock, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode %v, want 0600: only the daemon's own user (and root) may connect", perm)
	}

	if err := CallAPI(t.Context(), sock, "POST", "/api/pause"); err != nil {
		t.Fatalf("CallAPI over the socket: %v", err)
	}
	if !b.conf.Deref().Paused {
		t.Error("pause over the socket did not reach the daemon")
	}

	cancel()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context ended")
	}
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still present after shutdown: %v", err)
	}
}

// A socket left behind by a daemon that died without unlinking it
// answers nobody, and must not keep the next one from starting: under
// a supervisor that would be a restart loop with no way out.
func TestListenReplacesAStaleSocket(t *testing.T) {
	sock := filepath.Join(socketDir(t), "control.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("setup: stale socket missing: %v", err)
	}

	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	ln.Close()
}

// A socket something still answers on is another daemon's, and taking
// it over would leave two daemons planning against one pool.
func TestListenRefusesALiveSocket(t *testing.T) {
	sock := filepath.Join(socketDir(t), "control.sock")
	live, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	if ln, err := Listen(sock); err == nil {
		ln.Close()
		t.Fatal("Listen took over a socket another process is serving")
	}
}

// A socket that refuses this user — another user's daemon, its socket
// that user's alone — may be live, and only a refused connection says
// it is not. Root connects regardless, so as root the socket is simply
// live and refused as such.
func TestListenRefusesASocketItCannotProbe(t *testing.T) {
	sock := filepath.Join(socketDir(t), "control.sock")
	live, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := os.Chmod(sock, 0); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen(sock); err == nil {
		ln.Close()
		t.Fatal("Listen took over a socket it could not connect to")
	}
	if _, err := os.Lstat(sock); err != nil {
		t.Errorf("the unprobeable socket was removed: %v", err)
	}
}

// Whatever else is at the path is the operator's, not a stale socket,
// and is left alone.
func TestListenRefusesWhatIsNotASocket(t *testing.T) {
	path := filepath.Join(socketDir(t), "control.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err == nil {
		ln.Close()
		t.Fatal("Listen replaced a regular file")
	}
	if !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("refusal = %v, want it to say the path is not a socket", err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "not a socket" {
		t.Errorf("the file at the path was disturbed: %q, %v", body, err)
	}
}

// A TCP address is still a TCP address.
func TestListenOnATCPAddress(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ln.Addr().Network() != "tcp" {
		t.Errorf("listening on %s, want tcp", ln.Addr().Network())
	}
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", path); err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", path)
}
