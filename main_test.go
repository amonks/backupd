package main

import (
	"net"
	"testing"

	"monks.co/backupd/config"
	"monks.co/backupd/daemon"
)

// With no address configured the daemon opens no network listener: its
// control API authorizes nothing, so the one place it listens by
// default is a Unix socket only root reaches. A network address is the
// operator's to choose, in the config or with -addr.
func TestTheDefaultListenerIsNotANetworkOne(t *testing.T) {
	addr := listenAddr("", &config.Config{})
	if addr != daemon.DefaultListen {
		t.Fatalf("default listen address = %q, want %q", addr, daemon.DefaultListen)
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		t.Errorf("default listen address %q is a network address", addr)
	}

	if got := listenAddr("", &config.Config{Listen: "100.64.0.1:8888"}); got != "100.64.0.1:8888" {
		t.Errorf("configured listen = %q, want the config's", got)
	}
	if got := listenAddr("127.0.0.1:9999", &config.Config{Listen: "100.64.0.1:8888"}); got != "127.0.0.1:9999" {
		t.Errorf("-addr = %q, want it over the config's", got)
	}
}
