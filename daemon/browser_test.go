package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/device"
)

// The narrow and touch layouts are expressed entirely in CSS, and the
// rest of this package's tests can only read that CSS back as text.
// Text cannot answer the two questions that actually matter — does the
// page fit, and does the floor win the cascade — and both have been
// wrong here while a string test was green: a `font-size` floor that
// #config-editor outranked left the one control anybody types into
// under the zoom threshold, and the widest table on the dataset page
// went unrendered by every fixture. So this file asks a browser.
//
// It costs no new module in the mirror's go.sum: chromedp is already
// in it, under monks.co/pkg/datagrid, which is published and carries a
// browser suite of its own.

// browserLaunchTimeout is how long headless Chrome gets to print its
// "DevTools listening on ws://…" line, browserDialTimeout how long its
// DevTools socket then gets to accept the connection, browserBudget
// the wall-clock allowance for one test's whole session, and
// browserStep the allowance for a single waiting action inside it.
//
// All are ceilings, not costs, and deliberately loose: the first
// Chromium launch on a fresh CI builder cold-reads a few hundred MB
// from a slow image device (specs/ci.md § Chromium in the builder),
// which is why newBrowser pre-reads the install directory first. The
// per-step bound is what turns a stalled navigation into a named
// failure instead of an anonymous session-wide deadline (see CLAUDE.md).
const (
	browserLaunchTimeout = 2 * time.Minute
	browserDialTimeout   = time.Minute
	browserBudget        = 5 * time.Minute
	browserStep          = 30 * time.Second
)

// phone is the viewport every assertion here is made at: a small
// current handset. Mobile and Touch are what put `hover: none` in
// force, so the touch floor is live — measuring it any other way
// measures the desk layout and calls it a phone.
var phone = device.Info{
	Name: "backupd phone", Width: 390, Height: 844, Scale: 3,
	Mobile: true, Touch: true,
	UserAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) " +
		"AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
}

// spill is one element that reaches past the viewport with nothing to
// scroll it back.
type spill struct {
	What string `json:"what"`
	Past int    `json:"past"`
}

// pageFit is what the probe below reports about one rendered page.
type pageFit struct {
	Viewport int     `json:"viewport"`
	Overflow int     `json:"overflow"`
	Spilling []spill `json:"spilling"`
	Hiding   []spill `json:"hiding"`
	Small    []spill `json:"small"`
}

// fitProbe measures the three things a phone layout can get wrong.
//
// The page pans sideways when something ends past the viewport and no
// ancestor scrolls it back. The walk reports only the outermost
// offender of a nested run, so one wide cell does not print its whole
// ancestry.
//
// Content hides sideways when a box holds more than it shows. That is
// the failure the pan check alone misses, and it is the one the
// two-pane layout actually produced: `.main-content` carries
// `overflow-y: auto` in the standalone shell, and a box with one axis
// visible and the other not computes to `auto` on both — so squeezing
// the content pane to 120px did not widen the document, it quietly put
// 150px of the page behind a scrollbar nobody looks for. Exactly two
// things on these pages are meant to scroll sideways: a .table-scroll,
// and a grid inside its own wrap. Anything else that does is hiding
// something.
//
// A control zooms the page when its computed font-size is under 16px,
// the threshold at which a phone browser magnifies to meet a focused
// field and never comes back. Checkboxes and radios are exempt:
// nothing is typed into them, so neither zooms.
const fitProbe = `(() => {
  const viewport = document.documentElement.clientWidth;
  const past = el => el.getBoundingClientRect().right - viewport;
  const clipped = el => {
    for (let up = el.parentElement; up; up = up.parentElement) {
      if (getComputedStyle(up).overflowX !== "visible") return true;
    }
    return false;
  };
  const describe = el => {
    const id = el.id ? "#" + el.id : "";
    const cls = el.classList.length ? "." + [...el.classList].join(".") : "";
    const own = (el.textContent || "").trim().replace(/\s+/g, " ").slice(0, 48);
    return el.tagName.toLowerCase() + id + cls + " " + own;
  };
  const all = [...document.querySelectorAll(".backupd, .backupd *")];
  const spilling = all
    .filter(el => getComputedStyle(el).position !== "fixed")
    .filter(el => past(el) > 1 && !clipped(el))
    .filter(el => !el.parentElement || past(el.parentElement) <= 1)
    .map(el => ({what: describe(el), past: Math.round(past(el))}));
  const sanctioned = el => el.classList.contains("table-scroll") || !!el.closest("monks-datagrid");
  const hiding = all
    .filter(el => el.scrollWidth - el.clientWidth > 1 && !sanctioned(el))
    .map(el => ({what: describe(el), past: Math.round(el.scrollWidth - el.clientWidth)}));
  const typed = 'input:not([type="checkbox"]):not([type="radio"]), select, textarea';
  const small = [...document.querySelectorAll(".backupd " + typed)]
    .filter(el => el.getBoundingClientRect().width > 0)
    .map(el => ({what: describe(el), past: Math.round(parseFloat(getComputedStyle(el).fontSize))}))
    .filter(el => el.past < 16);
  return JSON.stringify({
    viewport: viewport,
    overflow: document.documentElement.scrollWidth - viewport,
    spilling: spilling,
    hiding: hiding,
    small: small,
  });
})()`

// TestBrowserPhoneLayout is the assertion the rest of this package's
// responsive tests stand in for: the dashboard fits a phone, and every
// control on it can be focused without the page zooming in for good.
//
// It reads the busy state deliberately — a dataset mid-backlog, so the
// plan table, its log lines and the transfer bar are all on the page.
// That is the state the layout is hardest in and the one a converged
// fixture never reaches.
func TestBrowserPhoneLayout(t *testing.T) {
	b := demoDaemon(t)
	server := httptest.NewServer(b.Handler())
	t.Cleanup(server.Close)

	ctx := newBrowser(t)
	for _, path := range []string{"/global", busyDataset, "/config"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			var raw string
			if err := chromedp.Run(ctx,
				step("emulate the phone", chromedp.Emulate(phone)),
				step("navigate to "+path, chromedp.Navigate(server.URL+path)),
				step("measure the layout", chromedp.Evaluate(fitProbe, &raw)),
			); err != nil {
				t.Fatal(err)
			}

			var fit pageFit
			if err := json.Unmarshal([]byte(raw), &fit); err != nil {
				t.Fatalf("probe returned %q: %v", raw, err)
			}
			if fit.Overflow > 1 {
				t.Errorf("the page pans %dpx sideways on a %dpx phone", fit.Overflow, fit.Viewport)
			}
			for _, s := range fit.Spilling {
				t.Errorf("%s ends %dpx past the edge with nothing to scroll it back", s.What, s.Past)
			}
			for _, s := range fit.Hiding {
				t.Errorf("%s holds %dpx more than it shows, and is not one of the two boxes "+
					"meant to scroll sideways", s.What, s.Past)
			}
			for _, s := range fit.Small {
				t.Errorf("%s is %dpx, under the 16px floor — focusing it zooms the page in and "+
					"nothing zooms it back out", s.What, s.Past)
			}
		})
	}
}

// step caps one action at its own allowance and names it. Chromedp's
// waiting actions are bounded by their context and nothing else, so an
// unbounded one spends the whole session budget and then reports
// "context deadline exceeded" naming no wait at all.
//
// The bound goes inside the action rather than around the Run: a Run
// on a derived context ties the browser target to that context, so
// cancelling it at the end of the first step closes the page every
// later step needs.
func step(what string, action chromedp.Action) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, browserStep)
		defer cancel()
		if err := action.Do(ctx); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
}

func chromePath(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		// On macOS, launching a Chrome.app binary through a symlink makes
		// its framework loader resolve relative paths from the wrong parent.
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		return path
	}
	t.Fatal("Chrome/Chromium is required for the backupd browser test")
	return ""
}

// warmChromeOnce guards the one pre-read of Chromium's install
// directory per test binary (see browserLaunchTimeout).
var warmChromeOnce sync.Once

func newBrowser(t *testing.T) context.Context {
	t.Helper()
	chrome := chromePath(t)
	warmChromeOnce.Do(func() {
		dir := filepath.Dir(chrome)
		// resources.pak sits beside every Linux Chrome and Chromium
		// binary; without it this is a shared bin directory (or macOS's
		// near-empty Contents/MacOS), where the pre-read would read a
		// lot and warm nothing.
		if _, err := os.Stat(filepath.Join(dir, "resources.pak")); err != nil {
			return
		}
		start := time.Now()
		read := warmPageCache(dir)
		t.Logf("pre-read %.1fMB under %s into the page cache in %s",
			float64(read)/(1<<20), dir, time.Since(start).Round(time.Millisecond))
	})
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(chrome),
			chromedp.UserDataDir(t.TempDir()),
			chromedp.Flag("headless", "new"),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.WSURLReadTimeout(browserLaunchTimeout),
		)...)
	t.Cleanup(cancelAlloc)
	ctx, cancelBrowser := chromedp.NewContext(allocCtx,
		chromedp.WithBrowserOption(chromedp.WithDialTimeout(browserDialTimeout)))
	t.Cleanup(cancelBrowser)
	ctx, cancelTimeout := context.WithTimeout(ctx, browserBudget)
	t.Cleanup(cancelTimeout)
	// Graceful shutdown before the allocator's kill, so Chrome has
	// released its user-data dir before t.TempDir's RemoveAll runs.
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = chromedp.Cancel(cleanup)
	})
	captureBrowserConsole(t, ctx)
	return ctx
}

// warmPageCache reads every regular file under dir so the kernel's page
// cache holds it, and returns the bytes read. Symlinks are skipped, as
// are unreadable entries: a warm-up is best-effort by design, never a
// reason to fail a test.
func warmPageCache(dir string) (read int64) {
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()
		n, _ := io.Copy(io.Discard, file)
		read += n
		return nil
	})
	return read
}

// captureBrowserConsole puts the page's own console output and its
// uncaught exceptions in the test log, where a layout failure caused by
// a script error is legible instead of mysterious.
func captureBrowserConsole(t *testing.T, ctx context.Context) {
	t.Helper()
	var mu sync.Mutex
	chromedp.ListenTarget(ctx, func(event any) {
		switch event := event.(type) {
		case *runtime.EventConsoleAPICalled:
			mu.Lock()
			defer mu.Unlock()
			parts := make([]string, 0, len(event.Args))
			for _, argument := range event.Args {
				if len(argument.Value) > 0 {
					parts = append(parts, string(argument.Value))
				} else if argument.Description != "" {
					parts = append(parts, argument.Description)
				}
			}
			t.Logf("browser console.%s: %s", event.Type, strings.Join(parts, " "))
		case *runtime.EventExceptionThrown:
			mu.Lock()
			defer mu.Unlock()
			t.Logf("browser exception: %s", event.ExceptionDetails.Error())
		}
	})
}
