package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/net"
)

// IndexEntry is one plugin in the official registry index. The index is a JSON
// array of these; it is the browse/discover surface for the admin UI.
type IndexEntry struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Kind    ext.Kind `json:"kind"`
	CoreAPI string   `json:"core_api"`
	SHA256  string   `json:"sha256"` // of the .tar.gz package at URL
	URL     string   `json:"url"`    // download URL of the package
	Sig     string   `json:"sig"`    // detached signature (base64), for out-of-band checks
	// Source is the name of the plugin source this entry came from ("official"
	// or an imported source's name). Set by AggregateIndex, not by the index
	// file itself.
	Source string `json:"source,omitempty"`
	// Provenance fields (Step 11). The official index lists OPEN-SOURCE plugins
	// only; FetchIndex drops any entry that is closed-source or paid. Proprietary/
	// paid plugins are installed only by URL/file, where the trust-tier gate
	// forces them through the 🔴 risk path.
	License   string `json:"license,omitempty"`
	SourceURL string `json:"source_url,omitempty"`
	Pricing   string `json:"pricing,omitempty"`
}

// isOpenSource reports whether an index entry is an open-source, free plugin —
// the only kind the official store may list. Mirrors ClosedOrPaid over the entry
// fields: a proprietary license, a missing source_url, or paid pricing excludes
// it.
func (e IndexEntry) isOpenSource() bool {
	return !ClosedOrPaid(ext.Manifest{Provenance: ext.Provenance{
		License:   e.License,
		SourceURL: e.SourceURL,
		Pricing:   e.Pricing,
	}})
}

// FetchIndex downloads and parses a plugin index from url. Filtering is the
// caller's concern: FetchOfficialIndex applies the official open-source-only
// rule.
//
// Privacy baseline (Step 2a): this is the ONLY outbound call in the plugin
// subsystem and it fires only when an admin explicitly requests the index with a
// non-empty configured URL. Nothing here runs at boot, so the zero-default-
// outbound guarantee holds; the setting `plugin_index_url` defaults empty.
func FetchIndex(ctx context.Context, url string) ([]IndexEntry, error) {
	if url == "" {
		return nil, fmt.Errorf("extension: plugin index url is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := net.HttpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("extension: fetch index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("extension: fetch index: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return nil, err
	}
	var entries []IndexEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("extension: parse index: %w", err)
	}
	return entries, nil
}

// FetchOfficialIndex fetches the OFFICIAL index and applies its listing rule:
// the official store is open-source only, so closed-source/paid entries are
// dropped. Imported sources are not subject to this rule — their closed/paid
// entries stay listed and simply classify as 🔴 at install.
func FetchOfficialIndex(ctx context.Context, url string) ([]IndexEntry, error) {
	entries, err := FetchIndex(ctx, url)
	if err != nil {
		return nil, err
	}
	open := entries[:0]
	for _, e := range entries {
		if e.isOpenSource() {
			open = append(open, e)
		}
	}
	return open, nil
}

// AggregateIndex fetches and merges the official index (when configured) and
// every imported source's index, attributing each entry to its source. A
// source that fails to fetch contributes a warning, not an error — one dead
// mirror must not blank the whole browse surface. Outbound calls happen only
// here, on explicit admin request (privacy baseline).
//
// Entries are not deduplicated across sources: the same ID may appear once
// per source (with different URLs/signatures), so consumers must key on
// (Source, ID).
func AggregateIndex(ctx context.Context, officialURL string) (entries []IndexEntry, warnings []string) {
	if officialURL != "" {
		es, err := FetchOfficialIndex(ctx, officialURL)
		if err != nil {
			warnings = append(warnings, OfficialSourceName+": "+err.Error())
		}
		for i := range es {
			es[i].Source = OfficialSourceName
		}
		entries = append(entries, es...)
	}
	for _, s := range CurrentSources() {
		es, err := FetchIndex(ctx, s.IndexURL)
		if err != nil {
			warnings = append(warnings, s.Name+": "+err.Error())
			continue
		}
		for i := range es {
			es[i].Source = s.Name
		}
		entries = append(entries, es...)
	}
	return entries, warnings
}
