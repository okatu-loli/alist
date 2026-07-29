package extension

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
)

// net.HttpClient() reads conf.Conf.TlsInsecureSkipVerify when it builds the
// shared client. Ensure it is populated for the fetch tests.
func ensureConf() {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
}

func TestFetchIndex(t *testing.T) {
	ensureConf()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// FetchIndex itself does not filter (Step 11 task 6): both entries come
		// back regardless of provenance. The open-source-only rule lives in
		// FetchOfficialIndex, tested separately.
		_, _ = w.Write([]byte(`[
			{"id":"com.example.a","name":"A","version":"1.0.0","kind":"StorageDriver","core_api":"^0.1.0","sha256":"abc","url":"https://x/a.tar.gz","sig":"sigA","license":"MIT","source_url":"https://github.com/example/a"},
			{"id":"com.example.b","name":"B","version":"2.0.0","kind":"OfflineDownloadTool","core_api":"^0.1.0","sha256":"def","url":"https://x/b.tar.gz","sig":"sigB","license":"Apache-2.0","source_url":"https://github.com/example/b"}
		]`))
	}))
	defer srv.Close()

	entries, err := FetchIndex(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "com.example.a" || entries[0].URL != "https://x/a.tar.gz" {
		t.Fatalf("entry 0 wrong: %+v", entries[0])
	}
}

func TestFetchIndexUnconfigured(t *testing.T) {
	if _, err := FetchIndex(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty index url")
	}
}

// TestFetchOfficialIndexDropsClosedSource proves the open-source-only filter
// (Step 11 task 6): a closed/paid entry must never appear in the official
// index result, so proprietary plugins can only be reached via manual URL
// install (the 🔴 risk path), not browsed from the store. FetchIndex itself
// no longer filters — that is FetchOfficialIndex's concern — so imported
// sources can list closed/paid entries too.
func TestFetchOfficialIndexDropsClosedSource(t *testing.T) {
	ensureConf()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"open.a","name":"A","version":"1.0.0","kind":"StorageDriver","core_api":"^0.1.0","sha256":"a","url":"https://x/a.tar.gz","sig":"s","license":"MIT","source_url":"https://github.com/x/a"},
			{"id":"proprietary.b","name":"B","version":"1.0.0","kind":"StorageDriver","core_api":"^0.1.0","sha256":"b","url":"https://x/b.tar.gz","sig":"s","license":"proprietary","source_url":"https://x/b"},
			{"id":"nosource.c","name":"C","version":"1.0.0","kind":"StorageDriver","core_api":"^0.1.0","sha256":"c","url":"https://x/c.tar.gz","sig":"s","license":"MIT"},
			{"id":"paid.d","name":"D","version":"1.0.0","kind":"StorageDriver","core_api":"^0.1.0","sha256":"d","url":"https://x/d.tar.gz","sig":"s","license":"MIT","source_url":"https://x/d","pricing":"paid"}
		]`))
	}))
	defer srv.Close()

	entries, err := FetchOfficialIndex(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchOfficialIndex: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "open.a" {
		t.Fatalf("expected only the open-source entry, got %+v", entries)
	}
}

func TestAggregateIndex(t *testing.T) {
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id":"a","name":"A","source_url":"https://g/a","license":"MIT"},
			{"id":"closed","name":"C","license":"proprietary","source_url":"https://g/c"}
		]`))
	}))
	defer official.Close()
	imported := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id":"b","name":"B","source_url":"https://g/b","license":"MIT"},
			{"id":"paid","name":"P","license":"MIT","source_url":"https://g/p","pricing":"paid"}
		]`))
	}))
	defer imported.Close()

	SetSourcesProvider(func() []PluginSource {
		return []PluginSource{{Name: "myrepo", IndexURL: imported.URL, PubKey: "00"}}
	})
	defer SetSourcesProvider(func() []PluginSource { return nil })

	entries, warns := AggregateIndex(context.Background(), official.URL)
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	ids := map[string]string{}
	for _, e := range entries {
		ids[e.ID] = e.Source
	}
	if _, ok := ids["closed"]; ok {
		t.Fatal("official index must stay open-source-only")
	}
	if ids["a"] != OfficialSourceName || ids["b"] != "myrepo" || ids["paid"] != "myrepo" {
		t.Fatalf("source attribution wrong: %v", ids)
	}
}

func TestAggregateIndexUnreachableSourceWarns(t *testing.T) {
	SetSourcesProvider(func() []PluginSource {
		return []PluginSource{{Name: "dead", IndexURL: "http://127.0.0.1:1/i.json", PubKey: "00"}}
	})
	defer SetSourcesProvider(func() []PluginSource { return nil })
	entries, warns := AggregateIndex(context.Background(), "")
	if len(entries) != 0 || len(warns) != 1 {
		t.Fatalf("want 0 entries 1 warning, got %d/%d", len(entries), len(warns))
	}
}

func TestFetchIndexBadStatus(t *testing.T) {
	ensureConf()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := FetchIndex(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error for 404")
	}
}
