package extension

import (
	"context"
	"fmt"
	"net"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// This file is the host consumption of Kind=AccessProtocol (design §2.1): the
// common floor beneath WebDAV/S3/MCP (TransportHTTP) and FTP/SFTP
// (TransportConn). It follows the F1 resolution idiom exactly (in-process
// instance registry first, then a gRPC factory seam) so a built-in/native
// protocol or a test double is delivered to without a subprocess, mirroring
// apiroute.go / eventhook.go.
//
// Security posture (design §0, §2.1): the host does NOT authorize here. It only
// routes inbound traffic to the plugin; the plugin then exchanges the inbound
// credential for a Principal via host.Auth() and does every fs op through
// host.FS(), which runs the core Authorize on each call. This file therefore
// adds no auth decision of its own — the decision stays in the F0 host layer.

// accessProtocolFactory is the gRPC-dispatch seam for a TransportHTTP protocol
// plugin (mirrors apiRouteFactory). nil until a gRPC bridge lands; in-process
// instances are the tested path. TransportConn protocols never use this — a raw
// net.Conn does not cross the gRPC boundary (design §5), so they are native only.
var accessProtocolFactory func(e *Entry) (ext.AccessProtocol, func(), error)

// SetAccessProtocolFactory installs the gRPC AccessProtocol provider (HTTP
// transport only).
func SetAccessProtocolFactory(fn func(e *Entry) (ext.AccessProtocol, func(), error)) {
	accessProtocolFactory = fn
}

var (
	accessProtocolMu        sync.RWMutex
	accessProtocolInstances = map[string]ext.AccessProtocol{}
)

// RegisterAccessProtocolInstance registers an in-process ext.AccessProtocol
// under id (a built-in/native protocol or a test double). A nil value
// deregisters.
func RegisterAccessProtocolInstance(id string, p ext.AccessProtocol) {
	accessProtocolMu.Lock()
	defer accessProtocolMu.Unlock()
	if p == nil {
		delete(accessProtocolInstances, id)
		return
	}
	accessProtocolInstances[id] = p
}

func resolveAccessProtocol(e *Entry) (ext.AccessProtocol, func()) {
	accessProtocolMu.RLock()
	if p, ok := accessProtocolInstances[e.Manifest.ID]; ok {
		accessProtocolMu.RUnlock()
		return p, func() {}
	}
	accessProtocolMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && accessProtocolFactory != nil {
		if p, release, err := accessProtocolFactory(e); err == nil {
			return p, release
		}
	}
	return nil, func() {}
}

// DispatchAccessProtocolHTTP forwards one inbound HTTP request to an enabled
// Kind=AccessProtocol plugin whose ProtocolInfo declares TransportHTTP
// (WebDAV/S3/MCP). It gates on enablement + Kind + transport before the plugin's
// ServeHTTP runs; the plugin then authenticates the request via host.Auth() and
// does its fs work via host.FS(), both still Grant-gated + Authorize-enforced by
// the F0 layer. This host layer adds NO auth decision — the decision stays in the
// host capability layer where Authorize runs on every operation.
func DispatchAccessProtocolHTTP(ctx context.Context, r *Registry, pluginID string, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	e, ok := r.Get(pluginID)
	// AccessProtocol has no FeatureBundle expansion path (only APIRoute/
	// EventHook/ScheduledJob/FileDecorator are AddKindIndex targets,
	// featurebundle.go), so the Manifest's declared Kinds is the sole truth
	// source here: e.HasKind would wrongly 404 a plugin that legitimately
	// declares AccessProtocol alongside another primary Kind (e.g. Kinds:
	// ["storage_driver", "access_protocol"]) — see DeclaresKind's doc.
	if !ok || !e.Enabled() || !e.DeclaresKind(ext.KindAccessProtocol) {
		return nil, ErrRouteNotDeclared
	}
	proto, release := resolveAccessProtocol(e)
	if proto == nil {
		return nil, ext.NewError(ext.ErrCodeNotImplement,
			fmt.Sprintf("AccessProtocol %q has no host-side handler wired", pluginID))
	}
	defer release()
	if proto.ProtocolInfo().Transport != ext.TransportHTTP {
		return nil, ext.NewError(ext.ErrCodeNotSupport,
			fmt.Sprintf("AccessProtocol %q is not an HTTP-transport protocol", pluginID))
	}
	return proto.ServeHTTP(ctx, req)
}

// ProtocolManager owns the inbound listeners for enabled TransportConn protocol
// plugins (FTP/SFTP). The host holds the listener — never the plugin — so the
// port/TLS come from settings and the plugin only ever receives an already
// accepted net.Conn (design §2.1). It mirrors the Scheduler's supervision shape:
// one accept goroutine per protocol, each accepted conn handled under a panic
// recover so one bad connection cannot take down the listener or the process.
type ProtocolManager struct {
	r  *Registry
	mu sync.Mutex
	// listeners are the bound listeners, kept so Stop can close them (which
	// unblocks the accept loops). addrs mirrors them keyed by plugin id so a
	// caller (and tests) can learn the actual bound address when DefaultPort was 0.
	listeners map[string]net.Listener
	wg        sync.WaitGroup
}

// NewProtocolManager builds a manager over r.
func NewProtocolManager(r *Registry) *ProtocolManager {
	return &ProtocolManager{r: r, listeners: map[string]net.Listener{}}
}

// Start binds a TCP listener for every enabled TransportConn AccessProtocol and
// begins accepting connections into the plugin's HandleConn. It returns after
// binding; ctx cancellation (or Stop) closes the listeners and stops the accept
// loops. A protocol whose listener fails to bind is logged and skipped — one
// protocol failing to bind never blocks the others.
func (m *ProtocolManager) Start(ctx context.Context) {
	for _, e := range m.r.ByKind(ext.KindAccessProtocol) {
		proto, release := resolveAccessProtocol(e)
		if proto == nil {
			continue
		}
		info := proto.ProtocolInfo()
		if info.Transport != ext.TransportConn {
			// HTTP-transport protocols are served through DispatchAccessProtocolHTTP
			// (mounted on the host router), not a dedicated listener.
			release()
			continue
		}
		addr := fmt.Sprintf(":%d", info.DefaultPort)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Warnf("extension: AccessProtocol %q listen %s failed: %v", e.Manifest.ID, addr, err)
			release()
			continue
		}
		m.mu.Lock()
		m.listeners[e.Manifest.ID] = ln
		m.mu.Unlock()
		m.wg.Add(1)
		go m.acceptLoop(ctx, e.Manifest.ID, proto, ln, release)
	}
}

// Addr returns the bound listener address for a TransportConn protocol (useful
// when DefaultPort was 0 / OS-assigned, and for tests). Nil if not bound.
func (m *ProtocolManager) Addr(pluginID string) net.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ln, ok := m.listeners[pluginID]; ok {
		return ln.Addr()
	}
	return nil
}

// Stop closes every listener (unblocking the accept loops) and waits for them to
// drain. Safe to call once.
func (m *ProtocolManager) Stop() {
	m.mu.Lock()
	for _, ln := range m.listeners {
		_ = ln.Close()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// acceptLoop accepts connections from ln and hands each to the plugin under a
// panic recover, until ctx is cancelled or the listener is closed.
func (m *ProtocolManager) acceptLoop(ctx context.Context, id string, proto ext.AccessProtocol, ln net.Listener, release func()) {
	defer m.wg.Done()
	defer release()
	// Close the listener when ctx is cancelled so a blocked Accept returns.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-stop:
		}
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Accept fails when the listener is closed (ctx cancel / Stop): exit.
			select {
			case <-ctx.Done():
				return
			default:
			}
			// A transient accept error also lands here; a closed listener is the
			// normal shutdown path, so just return.
			return
		}
		m.wg.Add(1)
		go m.handleConnOnce(ctx, id, proto, conn)
	}
}

// handleConnOnce runs one connection through the plugin with panic isolation and
// guarantees the conn is closed.
func (m *ProtocolManager) handleConnOnce(ctx context.Context, id string, proto ext.AccessProtocol, conn net.Conn) {
	defer m.wg.Done()
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("extension: AccessProtocol %q panicked handling conn: %v", id, r)
		}
	}()
	if err := proto.HandleConn(ctx, conn); err != nil {
		log.Warnf("extension: AccessProtocol %q HandleConn error: %v", id, err)
	}
}
