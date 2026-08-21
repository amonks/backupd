package daemon

import (
	"net/http"
	"strings"
)

// basePath is the path the dashboard is mounted at, as the browser sees
// it, taken from the X-Forwarded-Prefix a reverse proxy sets. It ends
// without a trailing slash, so every link is basePath + "/something"
// and a root mount yields exactly "/something".
//
// The daemon reads the header rather than its own URL because a proxy
// strips the prefix before forwarding: the request arrives at "/global"
// whether the browser asked for "/global" or "/backupd/global", and
// only the header says which.
func basePath(req *http.Request) string {
	return trimSlash(req.Header.Get("X-Forwarded-Prefix"))
}

// trimSlash normalizes a mount point to the empty string or a
// "/prefix" with no trailing slash.
func trimSlash(p string) string {
	p = strings.TrimSuffix(p, "/")
	if p == "/" {
		return ""
	}
	return p
}
