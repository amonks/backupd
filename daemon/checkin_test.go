package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

// TestTransportKind: a failed check-in is classified by its error chain,
// in the shapes net/http hands back — a *url.Error around the dial or
// read that failed — and never by its text.
func TestTransportKind(t *testing.T) {
	get := func(err error) error {
		return &url.Error{Op: "Get", URL: "https://nosnch.in/abc123", Err: err}
	}
	dial := func(errno syscall.Errno) error {
		return get(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)})
	}
	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{"the name did not resolve", get(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "nosnch.in", IsNotFound: true}}), "dns"},
		{"the resolver timed out", get(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "i/o timeout", Name: "nosnch.in", IsTimeout: true}}), "dns"},
		{"the connect timed out", dial(syscall.ETIMEDOUT), "timeout"},
		{"the context's deadline passed", get(fmt.Errorf("awaiting headers: %w", context.DeadlineExceeded)), "timeout"},
		{"nothing listened", dial(syscall.ECONNREFUSED), "connection-refused"},
		{"the peer reset the connection", get(&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}), "connection-reset"},
		{"there was no route", dial(syscall.EHOSTUNREACH), "network"},
		{"the certificate did not verify", get(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), "tls"},
		{"whatever answered spoke no tls", get(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}), "tls"},
		// The status is only in the text, and the text is the host's
		// check-in's to word.
		{"the snitch answered 503", errors.New("snitch abc123: 503 Service Unavailable"), ""},
		{"text that only reads like a timeout", errors.New("check-in timed out"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transportKind(tc.err); got != tc.kind {
				t.Errorf("transportKind(%v) = %q, want %q", tc.err, got, tc.kind)
			}
		})
	}
}
