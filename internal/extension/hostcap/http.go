package hostcap

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// OutboundPolicy is the host-global egress gate, consulted BEFORE a plugin's own
// Grant.Network (invariant 2 is the conjunction of the two). It models the
// privacy baseline "default no outbound" (design Step 2a): the zero value (a nil
// policy) denies everything. A deployment that opts into plugin egress installs
// a policy that permits specific hosts; the plugin can then only reach the
// intersection of that policy and its own granted allowlist.
type OutboundPolicy interface {
	Allow(host string) bool
}

// OutboundPolicyFunc adapts a func to OutboundPolicy.
type OutboundPolicyFunc func(host string) bool

func (f OutboundPolicyFunc) Allow(host string) bool { return f(host) }

// AllowAllOutbound is a policy that defers entirely to per-plugin Grant.Network
// (the host global gate is open, so the plugin's own allowlist is the only
// boundary). Use only where the deployment has decided plugin egress is
// governed solely by grants.
var AllowAllOutbound OutboundPolicy = OutboundPolicyFunc(func(string) bool { return true })

// hostHTTP is the outbound HTTP capability a plugin borrows. Do refuses any
// request whose target host is not permitted by BOTH the global OutboundPolicy
// AND the plugin's Grant.Network, before the request leaves the process.
type hostHTTP struct {
	grant  Grant
	policy OutboundPolicy
	client *http.Client
	// allowInternal is true only when the effective allowlist explicitly names an
	// internal target (a loopback/private literal, "localhost", or "*"). A plugin
	// granted only public hosts can never reach an internal IP — this is what
	// closes DNS-rebinding and SSRF-to-internal (e.g. the admin API on 127.0.0.1).
	allowInternal bool
}

// hostAllowed is the effective egress gate: BOTH the host-global policy AND the
// plugin's own Grant.Network must permit the host. Applied to the initial URL
// AND re-applied on every redirect hop (a 302 to an off-allowlist host was the
// VULN 3 bypass).
func (h *hostHTTP) hostAllowed(host string) bool {
	return h.policy != nil && h.policy.Allow(host) && h.grant.NetworkAllows(host)
}

func newHostHTTP(grant Grant, policy OutboundPolicy) *hostHTTP {
	h := &hostHTTP{grant: grant, policy: policy, allowInternal: computeAllowInternal(grant)}
	dialer := &net.Dialer{
		Timeout: 30 * time.Second,
		// Control runs AFTER DNS resolution, right before connect, so it sees the
		// real IP a hostname resolved to — the anti-rebinding checkpoint. An
		// allowlisted public host that resolves to an internal IP is refused
		// unless the grant explicitly opted into internal targets.
		Control: func(_, address string, _ syscall.RawConn) error {
			hostOnly, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(hostOnly); ip != nil && isInternalIP(ip) && !h.allowInternal {
				return ext.ErrPermissionDenied
			}
			return nil
		},
	}
	h.client = &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return ext.NewError(ext.ErrCodeNotSupport, "too many redirects")
			}
			if !h.hostAllowed(req.URL.Hostname()) {
				return ext.ErrPermissionDenied
			}
			return nil
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				hostOnly, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				// Defense in depth: the dialed host must also be on the allowlist
				// (CheckRedirect already gates hops, this guards the dial itself).
				if !h.hostAllowed(hostOnly) {
					return nil, ext.ErrPermissionDenied
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
	return h
}

// computeAllowInternal reports whether the plugin may reach internal
// (loopback/private/link-local) addresses at all: only when its granted
// allowlist explicitly names such a target. An admin granting "127.0.0.1" (a
// local aria2) or "*" (open egress) has opted in; a plugin granted only public
// hosts is blocked from every internal IP, which defeats rebinding/SSRF.
func computeAllowInternal(grant Grant) bool {
	for _, pat := range grant.Network {
		p := strings.ToLower(strings.TrimSpace(pat))
		if p == "*" || p == "localhost" || strings.HasSuffix(p, ".localhost") {
			return true
		}
		if ip := net.ParseIP(p); ip != nil && isInternalIP(ip) {
			return true
		}
	}
	return false
}

// isInternalIP reports whether ip is one a plugin must not reach unless
// explicitly granted: loopback, RFC1918/ULA private, link-local, or unspecified.
func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func (h *hostHTTP) Do(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	// Invariant 5: the plugin must have been granted the network capability.
	if !h.grant.HasAPI(CapNetwork) {
		return nil, ext.ErrPermissionDenied
	}
	if req == nil {
		return nil, ext.NewError(ext.ErrCodeNotSupport, "nil request")
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" {
		return nil, ext.NewError(ext.ErrCodeNotSupport, "invalid url")
	}
	// Invariant 2: global egress policy ∩ the plugin's granted allowlist. Both
	// must permit the host; either denial refuses the request. Re-checked on
	// every redirect hop (CheckRedirect) and at dial time (DialContext).
	if !h.hostAllowed(u.Hostname()) {
		return nil, ext.ErrPermissionDenied
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	hr, err := http.NewRequestWithContext(ctx, method, req.URL, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	resp, err := h.client.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &ext.HTTPResponse{Status: resp.StatusCode, Header: resp.Header, Body: respBody}, nil
}
