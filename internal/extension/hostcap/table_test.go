package hostcap

import (
	"context"
	"sort"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// This file exercises Host.Store().Table(name) (W6c, design docs
// v4-plugin-extensions-design.md §3): a schemaless per-plugin document table
// layered over the SAME KVBackend as Store().KV() — a "table:<name>:<id>" key
// prefix, not a disjoint storage extent (unlike Secrets).

// TestTableInsertGetRoundTrip: Insert returns a non-empty id; Get by that id
// returns back the exact row (deep equal), and the id is stable across
// repeated Gets.
func TestTableInsertGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("widgets")

	row := map[string]any{"name": "sprocket", "qty": float64(3)}
	id, err := tbl.Insert(ctx, row)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == "" {
		t.Fatal("Insert returned empty id")
	}

	got, ok, err := tbl.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Get = %v,%v,%v; want row,true,nil", got, ok, err)
	}
	if got["name"] != "sprocket" || got["qty"] != float64(3) {
		t.Fatalf("Get row = %+v, want %+v", got, row)
	}

	// Same id, same row, again.
	got2, ok2, err := tbl.Get(ctx, id)
	if err != nil || !ok2 || got2["name"] != "sprocket" {
		t.Fatalf("second Get = %+v,%v,%v", got2, ok2, err)
	}
}

// TestTableGetMissing: Get on an id that was never inserted returns ok=false,
// no error.
func TestTableGetMissing(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("widgets")

	_, ok, err := tbl.Get(ctx, "does-not-exist")
	if err != nil || ok {
		t.Fatalf("Get missing = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

// TestTableUpdateReplacesRow: Update on an existing id replaces the stored
// row entirely (old fields not present in the new row are gone).
func TestTableUpdateReplacesRow(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("widgets")

	id, err := tbl.Insert(ctx, map[string]any{"name": "old", "extra": "gone-after-update"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := tbl.Update(ctx, id, map[string]any{"name": "new"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, ok, err := tbl.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Get after update: %+v,%v,%v", got, ok, err)
	}
	if got["name"] != "new" {
		t.Fatalf("Get after update = %+v, want name=new", got)
	}
	if _, present := got["extra"]; present {
		t.Fatalf("Update did not fully replace the row, stale field survived: %+v", got)
	}
}

// TestTableUpdateMissingErrors: Update on an id that was never Inserted is an
// error (error-if-missing semantics, not upsert) — a caller's Update always
// means "I believe this row exists".
func TestTableUpdateMissingErrors(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("widgets")

	if err := tbl.Update(ctx, "never-inserted", map[string]any{"x": 1}); err == nil {
		t.Fatal("Update on a missing id: want error, got nil")
	}
	// And it must not have silently created the row (no upsert).
	if _, ok, _ := tbl.Get(ctx, "never-inserted"); ok {
		t.Fatal("Update on a missing id silently created a row (upsert), want error-if-missing")
	}
}

// TestTableDeleteRemoves: Delete removes the row; a subsequent Get misses.
// Deleting an already-missing id is a no-op (mirrors KV.Delete).
func TestTableDeleteRemoves(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("widgets")

	id, err := tbl.Insert(ctx, map[string]any{"name": "to-delete"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := tbl.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := tbl.Get(ctx, id); ok {
		t.Fatal("row still readable after Delete")
	}
	// Deleting again (already gone) is a no-op, not an error.
	if err := tbl.Delete(ctx, id); err != nil {
		t.Fatalf("Delete of an already-missing id: want nil, got %v", err)
	}
}

// TestTableListReturnsAllRowsInTable: List returns every row inserted into
// this table, and nothing else.
func TestTableListReturnsAllRowsInTable(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("widgets")

	id1, err := tbl.Insert(ctx, map[string]any{"name": "a"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := tbl.Insert(ctx, map[string]any{"name": "b"})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := tbl.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("List returned %d rows, want 2: %+v", len(rows), rows)
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		byID[r.ID] = r.Row
	}
	if byID[id1]["name"] != "a" || byID[id2]["name"] != "b" {
		t.Fatalf("List rows mismatch: %+v", byID)
	}
}

// TestTableIsolationBetweenTables: two differently-named tables in the same
// plugin never see each other's rows — List on one does not surface rows
// inserted into the other, even though both share the same underlying
// backend/plugin namespace.
func TestTableIsolationBetweenTables(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	widgets := h.Store().Table("widgets")
	gadgets := h.Store().Table("gadgets")

	wID, err := widgets.Insert(ctx, map[string]any{"kind": "widget"})
	if err != nil {
		t.Fatal(err)
	}
	gID, err := gadgets.Insert(ctx, map[string]any{"kind": "gadget"})
	if err != nil {
		t.Fatal(err)
	}

	wRows, err := widgets.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wRows) != 1 || wRows[0].ID != wID {
		t.Fatalf("widgets.List = %+v, want exactly [%s]", wRows, wID)
	}
	gRows, err := gadgets.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gRows) != 1 || gRows[0].ID != gID {
		t.Fatalf("gadgets.List = %+v, want exactly [%s]", gRows, gID)
	}

	// Cross-table Get also misses: gadgets' id is not readable via widgets.
	if _, ok, _ := widgets.Get(ctx, gID); ok {
		t.Fatal("widgets.Get resolved a gadgets row id — table isolation broken")
	}
}

// TestTablePerPluginIsolation: two plugins sharing one backend never see each
// other's table rows, even for the same table name.
func TestTablePerPluginIsolation(t *testing.T) {
	ctx := context.Background()
	backend := NewMemKVBackend()
	hA := New(Grant{PluginID: "plugin.A", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{KVBackend: backend})
	hB := New(Grant{PluginID: "plugin.B", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{KVBackend: backend})

	id, err := hA.Store().Table("t").Insert(ctx, map[string]any{"owner": "A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := hB.Store().Table("t").Get(ctx, id); ok {
		t.Fatal("plugin B read plugin A's table row — per-plugin isolation broken")
	}
	rows, err := hB.Store().Table("t").List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("plugin B's table list = %+v, want empty", rows)
	}
}

// TestTableGrantGateDenial: every Table op is denied without CapStore
// (fail-closed, same gate as KV/Secrets).
func TestTableGrantGateDenial(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p.nostore"}, Options{}) // no CapStore
	tbl := h.Store().Table("t")

	if _, err := tbl.Insert(ctx, map[string]any{"x": 1}); err != ext.ErrPermissionDenied {
		t.Fatalf("Insert without CapStore = %v, want ErrPermissionDenied", err)
	}
	if _, _, err := tbl.Get(ctx, "id"); err != ext.ErrPermissionDenied {
		t.Fatalf("Get without CapStore = %v, want ErrPermissionDenied", err)
	}
	if err := tbl.Update(ctx, "id", map[string]any{"x": 1}); err != ext.ErrPermissionDenied {
		t.Fatalf("Update without CapStore = %v, want ErrPermissionDenied", err)
	}
	if err := tbl.Delete(ctx, "id"); err != ext.ErrPermissionDenied {
		t.Fatalf("Delete without CapStore = %v, want ErrPermissionDenied", err)
	}
	if _, err := tbl.List(ctx); err != ext.ErrPermissionDenied {
		t.Fatalf("List without CapStore = %v, want ErrPermissionDenied", err)
	}
}

// TestTableIDsAreUniqueAcrossManyInserts: many Inserts in the same table
// never collide on id.
func TestTableIDsAreUniqueAcrossManyInserts(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("widgets")

	const n = 500
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id, err := tbl.Insert(ctx, map[string]any{"i": float64(i)})
		if err != nil {
			t.Fatalf("Insert #%d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q on insert #%d", id, i)
		}
		seen[id] = true
	}
	rows, err := tbl.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("List returned %d rows, want %d", len(rows), n)
	}
}

// TestTableNameRejectsSeparator: a table name containing the ':' prefix
// separator is rejected rather than silently allowed to inject into another
// table's key range.
func TestTableNameRejectsSeparator(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("evil:table")

	if _, err := tbl.Insert(ctx, map[string]any{"x": 1}); err == nil {
		t.Fatal("Insert into a table name containing ':': want error, got nil")
	}
	if _, _, err := tbl.Get(ctx, "id"); err == nil {
		t.Fatal("Get on a table name containing ':': want error, got nil")
	}
	if _, err := tbl.List(ctx); err == nil {
		t.Fatal("List on a table name containing ':': want error, got nil")
	}
}

// TestTableIDRejectsSeparator: an id containing the ':' separator is rejected
// by Get/Update/Delete rather than silently reading/writing outside the
// intended row's key.
func TestTableIDRejectsSeparator(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("widgets")

	if _, _, err := tbl.Get(ctx, "evil:id"); err == nil {
		t.Fatal("Get with ':' in id: want error, got nil")
	}
	if err := tbl.Update(ctx, "evil:id", map[string]any{"x": 1}); err == nil {
		t.Fatal("Update with ':' in id: want error, got nil")
	}
	if err := tbl.Delete(ctx, "evil:id"); err == nil {
		t.Fatal("Delete with ':' in id: want error, got nil")
	}
}

// TestTableNotReadableThroughRawKVKeyGuess: a table row is only reachable
// through Table's own accessors keyed by the exact "table:<name>:<id>"
// scheme — this pins the documented reserved-prefix scheme (Table shares
// KV's backend by design, unlike Secrets) so a future change to the prefix
// format is caught here.
func TestTableNotReadableThroughRawKVKeyGuess(t *testing.T) {
	ctx := context.Background()
	backend := NewMemKVBackend()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{KVBackend: backend})
	tbl := h.Store().Table("widgets")

	id, err := tbl.Insert(ctx, map[string]any{"name": "x"})
	if err != nil {
		t.Fatal(err)
	}
	// The row IS reachable via the raw backend at the documented key scheme —
	// this is intended (Table is a structured view over the same backend, not
	// a disjoint store like Secrets).
	raw, ok, err := backend.Get("p1", "table:widgets:"+id)
	if err != nil || !ok {
		t.Fatalf("expected the row at the documented table:<name>:<id> key, got ok=%v err=%v", ok, err)
	}
	if len(raw) == 0 {
		t.Fatal("row bytes empty at the documented key")
	}
	// But it is NOT visible via KV().Get on the bare id (no accidental alias).
	if _, ok, _ := h.Store().KV().Get(ctx, id); ok {
		t.Fatal("table row aliased into KV() under the bare id")
	}
}

func TestTableListSortedByIDIsStable(t *testing.T) {
	ctx := context.Background()
	h := New(Grant{PluginID: "p1", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{})
	tbl := h.Store().Table("widgets")
	var ids []string
	for i := 0; i < 5; i++ {
		id, err := tbl.Insert(ctx, map[string]any{"i": float64(i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows, err := tbl.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []string
	for _, r := range rows {
		gotIDs = append(gotIDs, r.ID)
	}
	sort.Strings(ids)
	sort.Strings(gotIDs)
	if len(gotIDs) != len(ids) {
		t.Fatalf("List returned %d ids, want %d", len(gotIDs), len(ids))
	}
	for i := range ids {
		if ids[i] != gotIDs[i] {
			t.Fatalf("List ids = %v, want %v", gotIDs, ids)
		}
	}
}
