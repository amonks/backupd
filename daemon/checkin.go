package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"os"
	"syscall"
	"time"
)

// checkIn pings Dead Man's Snitch after a successful cycle, and logs
// and journals how that went. It returns an error only when ctx ended
// during the ping.
//
// A failed ping is an expected failure (WARN), whatever went wrong: the
// snitch is a service across the public internet, so the ping fails
// now and then for reasons nobody here controls — the service, the
// uplink, a resolver — and the next one is an interval away. A ping
// that keeps failing needs no record of its own to be noticed, because
// a check-in that does not arrive is the very thing Dead Man's Snitch
// pages on, and the dashboard raises snitch-overdue. A ping our own
// stop cuts short did not fail at all, so it is neither logged nor
// journaled.
func (b *Daemon) checkIn(ctx context.Context, id string) error {
	err := b.snitch(ctx, id)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		attrs := []slog.Attr{
			slog.Bool(SnitchOKKey, false),
			slog.String(errorMessageKey, err.Error()),
		}
		if kind := transportKind(err); kind != "" {
			attrs = append(attrs, slog.String(errorTypeKey, kind))
		}
		b.log.LogAttrs(context.Background(), slog.LevelWarn, "snitch ping failed", attrs...)
		b.history.RecordSnitchError(time.Now(), err.Error())
		return nil
	}
	b.log.LogAttrs(context.Background(), slog.LevelInfo, "snitch pinged",
		slog.Bool(SnitchOKKey, true))
	b.history.RecordSnitch(time.Now())
	return nil
}

// transportKind is a failed check-in's error.type: what the error
// chain says went wrong on the way to the snitch, or "" when it does
// not say. It reads only typed causes, never the text, which Options
// leaves to whatever check-in the host supplies; an answer other than
// 2xx is in that text alone, so it goes unclassified. The checks run
// most specific first, so a DNS failure inside a dial error is dns,
// not network.
func transportKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return kindTimeout
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return kindDNS
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return kindTimeout
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return kindConnRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return kindConnReset
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return kindTLS
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return kindTLS
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return kindNetwork
	}
	return ""
}
