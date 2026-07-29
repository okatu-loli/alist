package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

func validManifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.demo",
		Name:    map[string]string{"en": "Demo"},
		Version: "1.2.3",
		CoreAPI: "^0.1.0", // compatible with host CoreAPIVersion 0.1.0
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		// Open-source, free provenance so a signature (not the closed/paid
		// override) decides the tier — the common case for an installable package.
		Provenance: ext.Provenance{
			Source:    "official",
			License:   "MIT",
			SourceURL: "https://github.com/example/demo",
			Pricing:   "free",
		},
	}
}

func TestValidateManifest(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ext.Manifest)
		wantErr bool
	}{
		{"valid", func(*ext.Manifest) {}, false},
		{"valid caret same", func(m *ext.Manifest) { m.CoreAPI = "^0.1.0" }, false},
		{"valid exact", func(m *ext.Manifest) { m.CoreAPI = "0.1.0" }, false},
		{"valid wildcard", func(m *ext.Manifest) { m.CoreAPI = "*" }, false},
		{"missing id", func(m *ext.Manifest) { m.ID = "" }, true},
		{"id path traversal", func(m *ext.Manifest) { m.ID = "../etc" }, true},
		{"id slash", func(m *ext.Manifest) { m.ID = "a/b" }, true},
		{"id backslash", func(m *ext.Manifest) { m.ID = "a\\b" }, true},
		{"id colon", func(m *ext.Manifest) { m.ID = "a:b" }, true},
		{"id dotdot embedded", func(m *ext.Manifest) { m.ID = "a..b" }, true},
		{"id leading dot", func(m *ext.Manifest) { m.ID = ".secrets" }, true},
		{"id valid reverse dns", func(m *ext.Manifest) { m.ID = "cn.alist.123open" }, false},
		{"id valid builtin", func(m *ext.Manifest) { m.ID = "builtin.web-dav_2" }, false},
		{"missing version", func(m *ext.Manifest) { m.Version = "" }, true},
		{"bad version", func(m *ext.Manifest) { m.Version = "not-semver" }, true},
		{"no kinds", func(m *ext.Manifest) { m.Kinds = nil }, true},
		{"unknown kind", func(m *ext.Manifest) { m.Kinds = []ext.Kind{"Nonsense"} }, true},
		// Every ext.Kind constant must be accepted by the host whitelist — a
		// manifest declaring one of these must not be rejected as "unknown kind".
		{"kind authnmethod", func(m *ext.Manifest) { m.Kinds = []ext.Kind{ext.KindAuthNMethod} }, false},
		{"kind accessprotocol", func(m *ext.Manifest) { m.Kinds = []ext.Kind{ext.KindAccessProtocol} }, false},
		{"kind featurebundle", func(m *ext.Manifest) { m.Kinds = []ext.Kind{ext.KindFeatureBundle} }, false},
		{"kind filedecorator", func(m *ext.Manifest) { m.Kinds = []ext.Kind{ext.KindFileDecorator} }, false},
		{"missing core_api", func(m *ext.Manifest) { m.CoreAPI = "" }, true},
		{"incompatible core_api v4", func(m *ext.Manifest) { m.CoreAPI = "^4.0.0" }, true},
		{"incompatible core_api next minor", func(m *ext.Manifest) { m.CoreAPI = "^0.2.0" }, true},
		{"incompatible exact", func(m *ext.Manifest) { m.CoreAPI = "0.2.0" }, true},
		{"public_prefix empty (no request)", func(m *ext.Manifest) { m.Contributions.PublicPrefix = "" }, false},
		{"public_prefix valid single segment", func(m *ext.Manifest) { m.Contributions.PublicPrefix = "/s" }, false},
		{"public_prefix valid with hyphen", func(m *ext.Manifest) { m.Contributions.PublicPrefix = "/my-share" }, false},
		{"public_prefix missing leading slash", func(m *ext.Manifest) { m.Contributions.PublicPrefix = "s" }, true},
		{"public_prefix multi segment", func(m *ext.Manifest) { m.Contributions.PublicPrefix = "/s/x" }, true},
		{"public_prefix uppercase", func(m *ext.Manifest) { m.Contributions.PublicPrefix = "/S" }, true},
		{"public_prefix underscore", func(m *ext.Manifest) { m.Contributions.PublicPrefix = "/my_share" }, true},
		{"public_prefix trailing slash", func(m *ext.Manifest) { m.Contributions.PublicPrefix = "/s/" }, true},
		// login_methods single-id addressing contract (Phase F): a manifest
		// declaring login_methods must also declare Kind=AuthNMethod, and every
		// declared login_methods[].id must equal the manifest's own id.
		{"login_methods without kind authnmethod", func(m *ext.Manifest) {
			m.Contributions.LoginMethods = []ext.LoginMethod{{ID: m.ID, Kind: "primary"}}
		}, true},
		{"login_methods id mismatch", func(m *ext.Manifest) {
			m.Kinds = append(m.Kinds, ext.KindAuthNMethod)
			m.Contributions.LoginMethods = []ext.LoginMethod{{ID: "some.other.id", Kind: "primary"}}
		}, true},
		{"login_methods valid", func(m *ext.Manifest) {
			m.Kinds = append(m.Kinds, ext.KindAuthNMethod)
			m.Contributions.LoginMethods = []ext.LoginMethod{{ID: m.ID, Kind: "primary"}}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			err := ValidateManifest(m)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	m := validManifest()
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.ID != m.ID || got.Version != m.Version {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// missing file
	if _, err := LoadManifest(t.TempDir()); err == nil {
		t.Fatal("expected error for missing manifest")
	}
}

func TestSatisfies(t *testing.T) {
	cases := []struct {
		have, constraint string
		want             bool
	}{
		{"0.1.0", "^0.1.0", true},
		{"0.1.5", "^0.1.0", true},
		{"0.2.0", "^0.1.0", false},
		{"0.1.0", "^0.2.0", false},
		{"0.1.0", "^4.0.0", false},
		{"0.1.0", "0.1.0", true},
		{"0.1.0", "0.1.1", false},
		{"0.1.0", "", true},
		{"1.2.3", "^1.0.0", true},
		{"2.0.0", "^1.0.0", false},
	}
	for _, c := range cases {
		got, err := satisfies(c.have, c.constraint)
		if err != nil {
			t.Fatalf("satisfies(%q,%q): %v", c.have, c.constraint, err)
		}
		if got != c.want {
			t.Errorf("satisfies(%q,%q)=%v want %v", c.have, c.constraint, got, c.want)
		}
	}
}
