package snitch

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingBody struct {
	reader *strings.Reader
	eof    bool
	closed bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err == io.EOF {
		b.eof = true
	}
	return n, err
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

func TestOKUsesContextAndDrainsAndClosesResponse(t *testing.T) {
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	type contextKey struct{}
	ctx := context.WithValue(t.Context(), contextKey{}, "caller")
	body := &trackingBody{reader: strings.NewReader("checked in")}
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Context().Value(contextKey{}); got != "caller" {
			t.Errorf("request context value = %v, want caller", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       body,
			Request:    req,
		}, nil
	})

	if err := OK(ctx, "test-snitch"); err != nil {
		t.Fatalf("OK: %v", err)
	}
	if !body.eof {
		t.Error("response body was not drained to EOF")
	}
	if !body.closed {
		t.Error("response body was not closed")
	}
}

func TestOKRejectsNon2xxAfterDrainingAndClosingResponse(t *testing.T) {
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	body := &trackingBody{reader: strings.NewReader("not found")}
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Body:       body,
			Request:    req,
		}, nil
	})

	err := OK(t.Context(), "test-snitch")
	if err == nil || !strings.Contains(err.Error(), "404 Not Found") {
		t.Fatalf("OK error = %v, want 404 failure", err)
	}
	if !body.eof {
		t.Error("response body was not drained to EOF")
	}
	if !body.closed {
		t.Error("response body was not closed")
	}
}
