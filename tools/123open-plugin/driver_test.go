package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// TestManifestMatchesPluginJSON guards the drift manifest()'s doc comment
// warns about: plugin.json (read by the install gateway when packaging) must
// stay semantically equivalent to manifest() (read by the running plugin's
// gRPC GetManifest RPC). Comparing the two decoded ext.Manifest values (rather
// than raw bytes) tolerates JSON formatting differences while still catching
// any field drift.
func TestManifestMatchesPluginJSON(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var fromFile ext.Manifest
	if err := json.Unmarshal(raw, &fromFile); err != nil {
		t.Fatalf("unmarshal plugin.json: %v", err)
	}
	fromCode := manifest()
	if !reflect.DeepEqual(fromFile, fromCode) {
		t.Fatalf("plugin.json and manifest() have drifted:\nplugin.json: %+v\nmanifest():  %+v", fromFile, fromCode)
	}
}
