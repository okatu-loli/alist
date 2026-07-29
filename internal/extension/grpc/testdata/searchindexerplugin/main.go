// Command searchindexerplugin serves a synthetic Kind=SearchIndexer extension
// as a go-plugin gRPC plugin (Phase E ring 1b's mechanism-proving test double
// — v4 ships no real third-party SearchIndexer plugin yet). Unlike
// offlinedownloadtoolplugin's echo-the-input trick (there is no state to hold
// across an AddURL/Status pair), this fixture keeps a real in-memory node
// table keyed by virtual path (path.Join(Parent, Name)), so Index/BatchIndex/
// Get/Del/Clear/Search behave enough to prove the gRPC round trip end to end —
// a Search after Index/BatchIndex must actually see what was indexed, and a
// Del must actually remove it. Mirrors offlinedownloadtoolplugin
// (testdata/offlinedownloadtoolplugin) one Kind over.
package main

import (
	"context"
	stdpath "path"
	"strings"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type searchIndexerPlugin struct {
	mu    sync.Mutex
	nodes map[string]ext.SearchNode
}

var _ ext.SearchIndexer = (*searchIndexerPlugin)(nil)

func (p *searchIndexerPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testsearchindexer",
		Name:    map[string]string{"en": "Test Search Indexer"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindSearchIndexer},
	}
}

func (p *searchIndexerPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *searchIndexerPlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *searchIndexerPlugin) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: "echoindexer", AutoUpdate: true}
}

func vpath(n ext.SearchNode) string { return stdpath.Join(n.Parent, n.Name) }

func (p *searchIndexerPlugin) Index(ctx context.Context, node ext.SearchNode) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodes[vpath(node)] = node
	return nil
}

func (p *searchIndexerPlugin) BatchIndex(ctx context.Context, nodes []ext.SearchNode) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range nodes {
		p.nodes[vpath(n)] = n
	}
	return nil
}

// Search matches on Keywords as a plain substring of the node name (no scope/
// paging semantics — this fixture only needs to prove that indexed data
// crosses the wire and back out through Search intact).
func (p *searchIndexerPlugin) Search(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ext.SearchNode
	for _, n := range p.nodes {
		if req.Keywords != "" && !strings.Contains(n.Name, req.Keywords) {
			continue
		}
		out = append(out, n)
	}
	return out, int64(len(out)), nil
}

func (p *searchIndexerPlugin) Get(ctx context.Context, parent string) ([]ext.SearchNode, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ext.SearchNode
	for _, n := range p.nodes {
		if n.Parent == parent {
			out = append(out, n)
		}
	}
	return out, nil
}

// Del removes the node at prefix plus every node whose virtual path is
// strictly below it (a subtree delete), mirroring ext.SearchIndexer.Del's
// contract.
func (p *searchIndexerPlugin) Del(ctx context.Context, prefix string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.nodes {
		if k == prefix || strings.HasPrefix(k, prefix+"/") {
			delete(p.nodes, k)
		}
	}
	return nil
}

func (p *searchIndexerPlugin) Clear(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodes = map[string]ext.SearchNode{}
	return nil
}

func main() {
	grpcplugin.ServeSearchIndexer(&searchIndexerPlugin{nodes: map[string]ext.SearchNode{}})
}
