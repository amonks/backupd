package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
)

// A listen address is either a TCP address ("100.x.y.z:8888") or the
// absolute path of a Unix socket ("/var/run/backupd/control.sock").
// A TCP address is reachable by everything that shares its network
// stack; a socket is reachable by whoever can reach its directory, and
// the daemon makes the socket itself its own user's alone (root
// bypasses that, as it bypasses everything). Where the control API
// should be no wider than "the operator and root on this machine", a
// socket in a directory nothing else can see is how to say so.
func socketPath(addr string) (string, bool) {
	if filepath.IsAbs(addr) {
		return addr, true
	}
	return "", false
}

// DefaultListen is where the daemon listens when nothing says
// otherwise: a Unix socket, which root alone reaches. The control API
// authorizes nothing, so the daemon never opens a network listener the
// operator did not ask for; a dashboard reachable from elsewhere is a
// `listen` (or -addr) naming a private address, or a forwarded socket.
const DefaultListen = "/var/run/backupd.sock"

// Listen opens the listener for a listen address: TCP for a host and
// port, a Unix socket for an absolute path. A socket file left behind
// by a daemon that died without removing it is replaced; one something
// still answers on, or anything at the path that is not a socket, is
// refused rather than taken over.
func Listen(addr string) (net.Listener, error) {
	path, ok := socketPath(addr)
	if !ok {
		return net.Listen("tcp", addr)
	}
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The mode is set after the bind, so for a moment the socket has
	// whatever the umask gave it; the directory is what keeps that
	// moment from mattering, and is the control worth relying on.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("restricting %s to its owner: %w", path, err)
	}
	return ln, nil
}

// clearStaleSocket removes a socket nothing is listening on. A daemon
// that exits cleanly removes its own; one that crashed leaves it, and
// binding over it fails with "address already in use" — under a
// supervisor, a restart loop with no way out.
func clearStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("listen %s: the path exists and is not a socket", path)
	}
	conn, err := net.Dial("unix", path)
	if err == nil {
		conn.Close()
		return fmt.Errorf("listen %s: another process is serving this socket", path)
	}
	// Only a refusal says nothing is listening. Any other failure —
	// EACCES from a socket another user's daemon owns, a full accept
	// queue — may be a live daemon, and taking its path over would
	// leave two planning against one pool.
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("listen %s: the socket exists and cannot be shown to be stale: %w", path, err)
	}
	return os.Remove(path)
}

// apiClient returns the client and base URL that reach a daemon's
// control API at addr.
func apiClient(addr string, client *http.Client) (*http.Client, string) {
	path, ok := socketPath(addr)
	if !ok {
		return client, "http://" + addr
	}
	var dialer net.Dialer
	client.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		},
	}
	// The host is a placeholder: the transport dials the socket
	// whatever the URL names.
	return client, "http://backupd"
}
