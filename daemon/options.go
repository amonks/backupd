package daemon

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"

	"monks.co/backupd/config"
	"monks.co/backupd/env"
)

// Options configure a Daemon. Only Config is required; every other
// field has a working default, so a host that wants the standalone
// daemon's behavior can pass Options{Config: conf} and nothing else.
type Options struct {
	// Config is the loaded configuration. Required.
	Config *config.Config

	// Env is the backing environment. Nil means env.New(Config): real
	// zfs, locally and over ssh. The sim package implements the same
	// seam in memory.
	Env env.Interface

	// Dryrun refreshes state and generates plans but executes no
	// transfers or deletions.
	Dryrun bool

	// Logger receives the daemon's structured record of what it does:
	// journal entries, snitch pings, and, with no Tracer, a record of
	// each unit of work. Nil means slog.Default(). New also routes the
	// in-memory ring buffers here (see logger.SetLogger), which is
	// process-wide.
	Logger *slog.Logger

	// Tracer records the daemon's units of work — sync cycles,
	// transfers, snapshot deletions, single-dataset syncs, snapshots —
	// as spans (see Tracer, and the span names beside CycleSpan). Nil
	// writes one record per ended span to Logger instead: its name as
	// the message, its attributes, StatusKey, DurationKey, and on a
	// failure error.message, unless the daemon Set one, at the
	// failure's level. A host with a tracing pipeline of its own passes
	// one, and gets spans in place of those records.
	//
	// Its Start is the only reader of the context a sync request hands
	// over (Daemon.TriggerSync): a requested sync's span starts under
	// the request's context, values and all — an HTTP host's request
	// carries its authority and its own event — and the work sees what
	// Start passes on. A host that wants a narrower parent may wrap.
	Tracer Tracer

	// Snitch checks a configured Dead Man's Snitch in after a
	// successful cycle. Nil uses the standalone daemon's HTTP client.
	// A host can replace it to integrate its own HTTP transport and
	// tracing; Run passes its caller context through unchanged.
	Snitch func(context.Context, string) error

	// Layout wraps every dashboard page in host-owned HTML. Nil means
	// backupd renders its own complete document.
	Layout Layout

	// OmitNav drops backupd's own top-level links (Overview, Config)
	// from the sidebar, for a host that renders them itself from
	// Nav(). Nothing else goes with them: the dataset list and the
	// global controls are built from the daemon's derived state, so no
	// host can render them, and a page without them is not the
	// dashboard.
	OmitNav bool
}

// Layout is a host-provided wrapper around each dashboard page. The
// daemon renders the page body into Page.Body and hands the result
// here; the layout is expected to write a complete HTML response. A
// non-nil error is reported to the client as a 500.
//
// Layout takes no templ or framework-specific types, so any Go code
// that can write HTML can be a layout.
type Layout func(w http.ResponseWriter, r *http.Request, page Page) error

// Page is the chrome-less rendering of one dashboard page, passed to a
// host-provided Layout. Body is a single .backupd element carrying the
// page's head assets (styles, the datagrid web component, the
// client-side script) and its content; every rule the daemon writes is
// scoped to that element, so a layout can embed it anywhere in its
// document without either stylesheet reaching the other. No user input
// is emitted unescaped.
//
// One thing a host does need to know about the body: it loads the
// data-grid custom element, which the tables are built from. The
// definition is guarded, so a host that already loads the same asset is
// fine, but a host loading a different build of it would win or lose by
// load order.
type Page struct {
	Title string
	Body  template.HTML
}

// NavLink is one of the daemon's own top-level pages. Path is relative
// to the mount point; a host rendering the nav itself joins it onto the
// request's base path with Link.
type NavLink struct {
	Label string
	Path  string
}

// Nav returns the daemon's top-level pages, in order. It exists for
// hosts using OmitNav, which render one unified nav from these plus
// their own links.
func Nav() []NavLink {
	return []NavLink{
		{Label: "Overview", Path: "/global"},
		{Label: "Config", Path: "/config"},
	}
}

// Link joins a base path with one of the daemon's own paths. A base
// path ends in a slash and a NavLink.Path begins with one, so
// concatenating them directly yields "//config" — which a browser reads
// as scheme-relative (host "config") when the daemon is mounted at the
// root, and costs a redirect per click under a prefix.
func Link(basePath, path string) string {
	return trimSlash(basePath) + path
}
