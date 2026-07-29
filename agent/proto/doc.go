// Package proto is the SHARED WIRE PROTOCOL for the AList agent: every type,
// constant, and pure function here is computed IDENTICALLY by both ends of a
// pairing/heartbeat/reverse-channel exchange — the alist server
// (internal/agentsrv, server/handles/agent_*.go) and the agent process
// (internal/agent, cmd/agent.go). It is the "one true copy" of the contract:
// fingerprinting (protocol.go), the reverse-channel cap-op method
// names and payload shapes (capproto.go), the two SAS derivations (sas.go,
// pairing_sas.go), the reverse-pairing token primitives (pairing_token.go), the
// token-authenticated first-contact handshake plus its WebSocket transport
// adapter (pairing_handshake.go, wsconn.go), and the wire types that used to be
// misfiled in agent-only files (wire.go: LogEntry, RapidUploadDetail,
// TaskStatus*).
//
// BOUNDARY RULES (a cycle bit this codebase before; these are enforced by
// convention here and by `go list -deps` in CI/the W3 acceptance check, not
// by the compiler alone):
//
//  1. proto MUST NOT import internal/agent/channel. channel is the lower,
//     transport-agnostic frame/session layer and knows nothing about pairing or
//     cap-ops; proto must stay a peer to it, never a client of it.
//  2. internal/agent/channel MUST NOT import proto, for the same reason in
//     reverse — channel is intentionally domain-agnostic.
//  3. proto MUST NOT import internal/agent (the agent-side runtime package) or
//     internal/agentsrv (the server-side runtime package). Both of THOSE import
//     proto; the reverse would be the exact cycle this split exists to avoid.
//  4. proto's only non-stdlib import is gorilla/websocket (wsconn.go's
//     transport adapter). It must never gain a dependency on this repo's
//     internal/db, internal/conf, internal/sign, or any other main-module
//     package — that is precisely the dependency closure this split removes
//     from the agent binary.
//
// Consequently the only two internal/agent/... imports that ever run "the
// other way" are the two runtime packages importing proto — never proto
// importing them.
package proto
