package daemon

import (
	"net/http"
	"strings"
)

// Mount returns a handler that serves the dashboard under a backend
// sub-path (e.g. "/backups") of a host's own mux. Handler expects to
// live at a root, so Mount presents it as if it did:
//
//   - Extends X-Forwarded-Prefix, so every link and fetch the dashboard
//     builds points back at the mounted location.
//   - Strips the sub-path before dispatching, so the internal routes
//     match.
//   - Prepends the sub-path to absolute backend Location headers, since
//     the dashboard redirects to paths like "/global" that would
//     otherwise escape the mount.
//
// Register it on the host mux at the same sub-path:
//
//	mux.Handle("/backups/", d.Mount("/backups"))
//
// A host behind a reverse proxy that already sets X-Forwarded-Prefix
// and strips the path — the usual deployment — needs none of this and
// should register Handler directly.
func (b *Daemon) Mount(prefix string) http.Handler {
	inner := b.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r := req.Clone(req.Context())
		r.Header.Set("X-Forwarded-Prefix", req.Header.Get("X-Forwarded-Prefix")+prefix)
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		inner.ServeHTTP(&locationPrefixer{ResponseWriter: w, prefix: prefix}, r)
	})
}

// locationPrefixer rewrites absolute backend Location headers on
// redirects so they stay inside the mount.
type locationPrefixer struct {
	http.ResponseWriter
	prefix string
}

func (w *locationPrefixer) WriteHeader(code int) {
	if code >= 300 && code < 400 {
		loc := w.Header().Get("Location")
		if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") &&
			!strings.HasPrefix(loc, w.prefix+"/") && loc != w.prefix {
			w.Header().Set("Location", w.prefix+loc)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *locationPrefixer) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
