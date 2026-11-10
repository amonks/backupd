package model

import "testing"

// TestReadMarksItsSide: adding a side's snapshots is reading that side,
// and only that side — a dataset first met on the local listing has a
// remote nobody has looked at yet, not an empty one.
func TestReadMarksItsSide(t *testing.T) {
	a := &Snapshot{Dataset: "/foo", Name: "daily-a", CreatedAt: 100}

	m := AddLocalDataset("/foo", []*Snapshot{a}, nil)(New())
	ds := m.GetDataset("/foo")
	if !ds.LocalKnown || ds.RemoteKnown {
		t.Fatalf("after a local read: local known %v, remote known %v; want true, false", ds.LocalKnown, ds.RemoteKnown)
	}

	m = AddRemoteDataset("/foo", nil, nil)(m)
	ds = m.GetDataset("/foo")
	if !ds.LocalKnown || !ds.RemoteKnown {
		t.Fatalf("after reading both sides: local known %v, remote known %v", ds.LocalKnown, ds.RemoteKnown)
	}
	if ds.Current.Remote.Len() != 0 {
		t.Fatalf("an empty read is an empty remote, got %d snapshots", ds.Current.Remote.Len())
	}

	clone := m.Clone().GetDataset("/foo")
	if !clone.LocalKnown || !clone.RemoteKnown {
		t.Fatal("a clone forgot what had been read")
	}
}

// TestListedMarksEveryDataset: a whole-side listing names every
// dataset that side holds, so a dataset it did not name holds nothing
// there — known, and empty.
func TestListedMarksEveryDataset(t *testing.T) {
	a := &Snapshot{Dataset: "/foo", Name: "daily-a", CreatedAt: 100}
	m := New()
	m = AddLocalDataset("/foo", []*Snapshot{a}, nil)(m)
	m = AddRemoteDataset("/gone", []*Snapshot{a}, nil)(m)

	m = Listed(Remote)(m)
	if !m.GetDataset("/foo").RemoteKnown {
		t.Error("/foo: a complete remote listing that did not name it should leave its remote known empty")
	}
	if m.GetDataset("/gone").LocalKnown {
		t.Error("/gone: marking the remote listed must not mark the local side")
	}

	m = Listed(Local)(m)
	if gone := m.GetDataset("/gone"); !gone.LocalKnown || gone.Current.Local.Len() != 0 {
		t.Errorf("/gone: a complete local listing that did not name it should leave its local known empty, got known %v with %d", gone.LocalKnown, gone.Current.Local.Len())
	}
}

// TestKeepRemote: a model whose remote has not been read carries the
// remote side of the model before it, for each dataset that model had
// read the remote of. A remote that could not be listed is not a
// remote that emptied.
func TestKeepRemote(t *testing.T) {
	a := &Snapshot{Dataset: "/foo", Name: "daily-a", CreatedAt: 100}
	b := &Snapshot{Dataset: "/foo", Name: "daily-b", CreatedAt: 200}
	size := &DatasetSize{Used: 70}

	prev := New()
	prev = AddLocalDataset("/foo", []*Snapshot{a}, nil)(prev)
	prev = AddRemoteDataset("/foo", []*Snapshot{a}, size)(prev)
	prev = AddRemoteDataset("/remote-only", []*Snapshot{a}, nil)(prev)
	prev = AddLocalDataset("/unread", []*Snapshot{a}, nil)(prev)

	next := New()
	next = AddLocalDataset("/foo", []*Snapshot{a, b}, nil)(next)
	next = AddLocalDataset("/unread", []*Snapshot{a}, nil)(next)
	next = AddLocalDataset("/new", []*Snapshot{b}, nil)(next)

	got := KeepRemote(prev)(next)

	foo := got.GetDataset("/foo")
	if !foo.RemoteKnown || !foo.Current.Remote.Has(a) || foo.Current.Remote.Len() != 1 {
		t.Errorf("/foo: expected the previous remote inventory, got known %v, %s", foo.RemoteKnown, foo)
	}
	if !foo.Metrics.HasRemote || foo.Metrics.RemoteSize.Used != 70 {
		t.Errorf("/foo: expected the previous remote size, got %+v", foo.Metrics)
	}
	if foo.Current.Local.Len() != 2 {
		t.Errorf("/foo: the fresh local read must stand, got %d local snapshots", foo.Current.Local.Len())
	}
	if ro := got.GetDataset("/remote-only"); ro == nil || !ro.RemoteKnown || ro.Current.Remote.Len() != 1 {
		t.Errorf("/remote-only: expected carried, got %v", ro)
	}
	if u := got.GetDataset("/unread"); u.RemoteKnown {
		t.Error("/unread: a remote the previous model never read is still unread")
	}
	if n := got.GetDataset("/new"); n.RemoteKnown {
		t.Error("/new: a dataset the previous model never saw has an unread remote")
	}
	if next.GetDataset("/foo").RemoteKnown {
		t.Error("KeepRemote must not mutate the model it is given")
	}
}

// TestReplaceRemote: a whole remote listing replaces the remote side of
// the model it is published into — what it named takes its snapshots
// and size, the rest holds nothing there — and leaves the local side as
// that model has it.
func TestReplaceRemote(t *testing.T) {
	a := &Snapshot{Dataset: "/foo", Name: "daily-a", CreatedAt: 100}
	b := &Snapshot{Dataset: "/foo", Name: "daily-b", CreatedAt: 200}

	cur := New()
	cur = AddLocalDataset("/foo", []*Snapshot{a, b}, nil)(cur)
	cur = AddRemoteDataset("/foo", []*Snapshot{a}, &DatasetSize{Used: 10})(cur)
	cur = AddLocalDataset("/gone-remote", []*Snapshot{a}, nil)(cur)
	cur = AddRemoteDataset("/gone-remote", []*Snapshot{a}, &DatasetSize{Used: 10})(cur)

	listing := New()
	listing = AddRemoteDataset("/foo", []*Snapshot{a, b}, &DatasetSize{Used: 20})(listing)
	listing = AddRemoteDataset("/remote-only", []*Snapshot{a}, nil)(listing)

	got := ReplaceRemote(listing)(cur)

	foo := got.GetDataset("/foo")
	if foo.Current.Remote.Len() != 2 || foo.Metrics.RemoteSize.Used != 20 || !foo.RemoteKnown {
		t.Errorf("/foo: expected the listing's remote, got %s", foo)
	}
	if foo.Current.Local.Len() != 2 || !foo.LocalKnown {
		t.Errorf("/foo: the local side must stand, got %s", foo)
	}
	gone := got.GetDataset("/gone-remote")
	if gone.Current.Remote.Len() != 0 || gone.Metrics.HasRemote || !gone.RemoteKnown {
		t.Errorf("/gone-remote: a dataset the listing did not name holds nothing remotely, got %s %+v", gone, gone.Metrics)
	}
	ro := got.GetDataset("/remote-only")
	if ro == nil || ro.Current.Remote.Len() != 1 || ro.LocalKnown {
		t.Errorf("/remote-only: expected added with its local side unread, got %v", ro)
	}
	if cur.GetDataset("/foo").Current.Remote.Len() != 1 {
		t.Error("ReplaceRemote must not mutate the model it is given")
	}
}

func TestRetain(t *testing.T) {
	m := New()
	m = AddLocalDataset("/a", nil, nil)(m)
	m = AddLocalDataset("/b", nil, nil)(m)
	got := Retain("/a", "/c")(m)
	if got.GetDataset("/a") == nil || got.GetDataset("/b") != nil || got.GetDataset("/c") != nil {
		t.Errorf("expected only /a, got %v", got.ListDatasets())
	}
	if m.GetDataset("/b") == nil {
		t.Error("Retain must not mutate the model it is given")
	}
}
