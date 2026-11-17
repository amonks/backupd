package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chromedp/chromedp"

	"monks.co/pkg/browsertest"
)

// TestBrowserDashboardIsBusyWhileItsRefreshIsUnderWay holds the live
// refresh to the settle contract (specs/pkg-browsertest.md § Settled):
// once the long poll reports a change, the dashboard is aria-busy until
// the refetched body is swapped in, and settled before and after.
func TestBrowserDashboardIsBusyWhileItsRefreshIsUnderWay(t *testing.T) {
	b := demoDaemon(t)
	dashboard := nonced(b.Handler())
	// One token per change: the poll that takes it reports the change,
	// and the ones after it are held again.
	changed := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/poll":
			// The test says when the state changed; until then the poll
			// is held, as the daemon holds it.
			select {
			case <-changed:
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
			}
		case r.Header.Get("Sec-Fetch-Dest") == "empty":
			// The refresh's refetch of the page, held until released.
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			dashboard.ServeHTTP(w, r)
		default:
			dashboard.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	ctx := browsertest.NewBrowser(t)
	const busy = `document.querySelector(".backupd").getAttribute("aria-busy") === "true"`
	if err := chromedp.Run(ctx, browsertest.Open(server.URL+"/global")); err != nil {
		t.Fatal(err)
	}
	changed <- struct{}{}
	if err := chromedp.Run(ctx, browsertest.Poll(busy)); err != nil {
		t.Fatal(err)
	}
	close(release)
	released = true
	if err := chromedp.Run(ctx, browsertest.Settled()); err != nil {
		t.Fatal(err)
	}
}
