package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/device"

	"regexp"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"monks.co/backupd/history"
	"monks.co/pkg/browsertest"
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
// The launch, the page-cache pre-read, the console capture and the
// per-step bounds are monks.co/pkg/browsertest's, shared with every
// browser suite in the fleet. It is published, so the mirror resolves
// it with no workspace to fall back on, as it already does for
// chromedp — which it carries anyway, under monks.co/pkg/datagrid.

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

	ctx := browsertest.NewBrowser(t)
	for _, path := range []string{"/global", busyDataset, "/config"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			var raw string
			if err := chromedp.Run(ctx,
				browsertest.Step("emulate the phone", chromedp.Emulate(phone)),
				browsertest.Step("navigate to "+path, chromedp.Navigate(server.URL+path)),
				browsertest.Step("measure the layout", chromedp.Evaluate(fitProbe, &raw)),
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

// A countdown is a ticker: the page's formatUntil says what the server's
// fmtCountdown says (countdownCases), so the first tick never changes the
// words, and it counts down one second at a time. Chrome's virtual clock
// drives its real timers: a busy builder can delay a wall-clock callback
// past several seconds, correctly skipping numbers. Go's synctest clock
// cannot control the separate browser process.
func TestBrowserCountdownTicksInTheServersWords(t *testing.T) {
	b := demoDaemon(t)
	server := httptest.NewServer(b.Handler())
	t.Cleanup(server.Close)
	ctx := browsertest.NewBrowser(t)
	browsertest.CaptureConsole(t, ctx)
	if err := chromedp.Run(ctx,
		browsertest.Step("open the overview", chromedp.Navigate(server.URL+"/global")),
		browsertest.Step("pause the browser clock", chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := emulation.SetVirtualTimePolicy(emulation.VirtualTimePolicyPause).Do(ctx)
			return err
		})),
	); err != nil {
		t.Fatal(err)
	}
	for _, c := range countdownCases {
		var got string
		if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`formatUntil(%d)`, c.d.Milliseconds()), &got)); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("formatUntil(%s) = %q, the server says %q", c.d, got, c.want)
		}
	}
	var seen []struct {
		Text string  `json:"text"`
		At   float64 `json:"at"`
	}
	advanced := make(chan struct{}, 1)
	chromedp.ListenTarget(ctx, func(event any) {
		if _, ok := event.(*emulation.EventVirtualTimeBudgetExpired); ok {
			advanced <- struct{}{}
		}
	})
	if err := chromedp.Run(ctx,
		browsertest.Step("add a countdown", chromedp.Evaluate(`(() => {
			const el = document.createElement('span');
			el.dataset.until = String(Date.now() + 9500);
			// The first long-poll can refresh the dashboard's children.
			// Keep the probe outside that region so it stays connected.
			document.body.append(el);
			window.ticks = [];
			new MutationObserver(() => {
				const last = window.ticks[window.ticks.length - 1];
				if (!last || last.text !== el.textContent) window.ticks.push({text: el.textContent, at: performance.now()});
			}).observe(el, {subtree: true, childList: true, characterData: true});
		})()`, nil)),
		browsertest.Step("advance the browser clock", chromedp.ActionFunc(func(ctx context.Context) error {
			// The dashboard keeps a long-poll pending. Advance even while
			// fetching; pauseIfNetworkFetchesPending would never tick.
			if _, err := emulation.SetVirtualTimePolicy(emulation.VirtualTimePolicyAdvance).WithBudget(6500).Do(ctx); err != nil {
				return err
			}
			select {
			case <-advanced:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})),
		chromedp.Evaluate(`window.ticks`, &seen),
	); err != nil {
		t.Fatal(err)
	}
	if len(seen) < 5 {
		t.Fatalf("countdown did not tick each second: %+v", seen)
	}
	prev := 0
	for i, s := range seen {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(s.Text, "in "), "s"))
		if err != nil {
			t.Fatalf("countdown %d = %q: %+v", i, s.Text, seen)
		}
		if i > 0 {
			if n != prev-1 {
				t.Fatalf("countdown went from %ds to %ds: %+v", prev, n, seen)
			}
			// The first observation discovers the added element; later
			// changes are aligned to its deadline's whole seconds.
			if gap := s.At - seen[i-1].At; i > 1 && (gap < 999 || gap > 1001) {
				t.Fatalf("countdown %d came %.0fms after the last: %+v", i, gap, seen)
			}
		}
		prev = n
	}
}

// The cycle strip's dots compose their hover — start · outcome — from
// data through the formatter pkg/localtime leaves on the window, since
// localtime's own title rewrite would replace the whole title with the
// stamp; and the ordinary "ago" hovers take that rewrite. Both read in
// the viewer's zone, pinned here away from the host's.
func TestBrowserHoversReadInTheViewersZone(t *testing.T) {
	b := demoDaemon(t)
	start := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	b.history.RecordCycle(history.Cycle{StartedAt: start, StoppedAt: start.Add(3 * time.Second), OK: true, Datasets: 1})
	server := httptest.NewServer(b.Handler())
	t.Cleanup(server.Close)

	ctx := browsertest.NewBrowser(t)
	var dot, ago string
	if err := chromedp.Run(ctx,
		browsertest.Step("pin the viewer's zone", emulation.SetTimezoneOverride("America/Los_Angeles")),
		browsertest.Step("open the overview", chromedp.Navigate(server.URL+"/global")),
		browsertest.Step("read a dot's hover", chromedp.Evaluate(`document.querySelector('.qdot[data-at]').title`, &dot)),
		browsertest.Step("read an ago hover", chromedp.Evaluate(`document.querySelector('time[data-localtime-title]').title`, &ago)),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dot, "2026-01-15 04:00:00 PST · ok in 3s") {
		t.Errorf("dot title = %q, want the start in the viewer's zone then the outcome", dot)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d P[DS]T$`).MatchString(ago) {
		t.Errorf("ago title = %q, want the instant in the viewer's zone", ago)
	}
}
