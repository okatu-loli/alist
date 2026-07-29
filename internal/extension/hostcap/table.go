package hostcap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	ext "github.com/AlistGo/alist-ext"
)

// This file implements Host.Store().Table(name) (W6c, design docs
// v4-plugin-extensions-design.md §3): a schemaless per-plugin document table
// — named tables of JSON-document rows addressed by a host-assigned primary
// key — layered over the SAME KVBackend as Store().KV(), not a disjoint
// storage extent like Secrets (secrets.go's doc comment explains why Secrets
// needs disjoint storage; Table has no such requirement — it holds whatever
// plugin-private data the plugin could already write through KV(), just
// structured as rows instead of opaque blobs).
//
// Isolation scheme: every table row lives under the key
//
//	"table:" + <table-name> + ":" + <id>
//
// within the plugin's existing KVBackend namespace (per-plugin isolation is
// therefore inherited from KVBackend itself, unchanged from KV()). "table:"
// is a RESERVED key prefix: a plugin using Table() must not pick a table name
// containing ':' (validateTableComponent rejects it), and an id is always the
// host-generated hex token from newTableID (also never contains ':') — so no
// table name or id can be crafted to inject the ':' separator and forge a key
// that lands in another table's (or another id's) range. A plain KV().Set
// call with a key that happens to start with "table:" is a caller footgun we
// do not defend against (documented, not enforced): Table is a structured
// VIEW over the same backend, so this is the intended, documented trade-off
// against Secrets' stronger (disjoint-storage) isolation.

const tableKeyPrefix = "table:"

// validateTableComponent rejects a table name or id that could inject the
// "table:<name>:<id>" separator: empty, or containing ':'.
func validateTableComponent(kind, s string) error {
	if s == "" {
		return fmt.Errorf("hostcap: table %s must not be empty", kind)
	}
	if strings.Contains(s, ":") {
		return fmt.Errorf("hostcap: table %s %q must not contain ':'", kind, s)
	}
	return nil
}

// tableRowKey builds the backend key for one row. Callers must have already
// validated name and id via validateTableComponent.
func tableRowKey(name, id string) string {
	return tableKeyPrefix + name + ":" + id
}

// tableRowPrefix builds the scan prefix for every row in name. Callers must
// have already validated name.
func tableRowPrefix(name string) string {
	return tableKeyPrefix + name + ":"
}

// newTableID mints a host-assigned primary key: 16 random bytes (crypto/rand,
// not a counter or wall-clock timestamp) hex-encoded to 32 characters. Never
// contains ':', so it can never be mistaken for (or inject into) the
// "table:<name>:<id>" separator scheme.
func newTableID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("hostcap: generate table row id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// hostTable is the ext.Table a plugin borrows for one table name, bound to
// the plugin id + grant gate (same CapStore gate as KV/Secrets) and backed by
// the SAME KVBackend as hostKV — see the isolation scheme above.
type hostTable struct {
	pluginID string
	name     string
	granted  bool
	backend  KVBackend
}

var _ ext.Table = (*hostTable)(nil)

func (t *hostTable) Insert(_ context.Context, row map[string]any) (string, error) {
	if !t.granted {
		return "", ext.ErrPermissionDenied
	}
	if err := validateTableComponent("name", t.name); err != nil {
		return "", err
	}
	id, err := newTableID()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(row)
	if err != nil {
		return "", fmt.Errorf("hostcap: marshal table row: %w", err)
	}
	if err := t.backend.Set(t.pluginID, tableRowKey(t.name, id), data); err != nil {
		return "", err
	}
	return id, nil
}

func (t *hostTable) Get(_ context.Context, id string) (map[string]any, bool, error) {
	if !t.granted {
		return nil, false, ext.ErrPermissionDenied
	}
	if err := validateTableComponent("name", t.name); err != nil {
		return nil, false, err
	}
	if err := validateTableComponent("id", id); err != nil {
		return nil, false, err
	}
	data, ok, err := t.backend.Get(t.pluginID, tableRowKey(t.name, id))
	if err != nil || !ok {
		return nil, ok, err
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		return nil, false, fmt.Errorf("hostcap: unmarshal table row: %w", err)
	}
	return row, true, nil
}

func (t *hostTable) Update(_ context.Context, id string, row map[string]any) error {
	if !t.granted {
		return ext.ErrPermissionDenied
	}
	if err := validateTableComponent("name", t.name); err != nil {
		return err
	}
	if err := validateTableComponent("id", id); err != nil {
		return err
	}
	key := tableRowKey(t.name, id)
	_, ok, err := t.backend.Get(t.pluginID, key)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("hostcap: table %q: no row with id %q (Update is error-if-missing, not upsert)", t.name, id)
	}
	data, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("hostcap: marshal table row: %w", err)
	}
	return t.backend.Set(t.pluginID, key, data)
}

func (t *hostTable) Delete(_ context.Context, id string) error {
	if !t.granted {
		return ext.ErrPermissionDenied
	}
	if err := validateTableComponent("name", t.name); err != nil {
		return err
	}
	if err := validateTableComponent("id", id); err != nil {
		return err
	}
	return t.backend.Delete(t.pluginID, tableRowKey(t.name, id))
}

func (t *hostTable) List(_ context.Context) ([]ext.TableRow, error) {
	if !t.granted {
		return nil, ext.ErrPermissionDenied
	}
	if err := validateTableComponent("name", t.name); err != nil {
		return nil, err
	}
	prefix := tableRowPrefix(t.name)
	keys, err := t.backend.List(t.pluginID, prefix)
	if err != nil {
		return nil, err
	}
	sort.Strings(keys) // deterministic order, not part of the interface contract
	rows := make([]ext.TableRow, 0, len(keys))
	for _, k := range keys {
		data, ok, err := t.backend.Get(t.pluginID, k)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // raced with a concurrent Delete; skip rather than error
		}
		var row map[string]any
		if err := json.Unmarshal(data, &row); err != nil {
			return nil, fmt.Errorf("hostcap: unmarshal table row %q: %w", k, err)
		}
		rows = append(rows, ext.TableRow{ID: strings.TrimPrefix(k, prefix), Row: row})
	}
	return rows, nil
}
