// Package alistagent is the module root of github.com/AlistGo/alist-agent —
// the remote AList agent: the wire protocol both ends of a
// pairing/heartbeat/reverse-channel exchange share (proto/), the
// transport-agnostic frame/session layer under it (channel/), and the
// agent-side runtime that runs as its own process (agent/). It has no
// exported API of its own; import one of those three subpackages instead.
//
// STABILITY: v0.x — UNSTABLE. This module is split out of the AList core; the
// wire format and Go API may change on minor version bumps until it settles.
//
// BOUNDARY RULE: this module MUST NOT import anything from the core's
// internal/ tree (or any other core package). It is a standalone Go module
// (its own go.mod; the core module pulls it in through a replace directive
// during in-tree development, exactly like ./ext) precisely so the agent can
// be built, tested, and eventually released independently of the AList
// server. Within the module itself: proto/ and channel/ MUST NOT import each
// other (channel is a domain-agnostic transport layer; proto is a peer, not a
// client, of it — see proto/doc.go rules 1-2); agent/ imports both but
// neither imports back; cli/ (the join/run/serve cobra command tree, mounted
// by both this repo's agent/cmd/alist-agent and the core's cmd/agent.go
// shell — design analysis §6 W5) imports agent/ + channel/ + proto/ but
// nothing imports back into it. Keep third-party dependencies limited to
// what the design analysis identifies as load-bearing: gorilla/websocket,
// Xhofe/go-cache, sirupsen/logrus, golang.org/x/crypto, and — for cli/ only
// — spf13/cobra.
//
// LICENSE: unlike ext/ (a frozen SDK boundary released under a permissive
// license), this module is a complete, independently runnable program — not
// merely an API surface extensions link against — so it carries the same
// license as the core it was split from: AGPL-3.0 (see ./LICENSE).
package alistagent
