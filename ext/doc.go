// Package ext is the frozen public SDK boundary between the AList core (the
// "host") and its extensions — storage drivers today, and (reserved) the other
// extension Kinds tomorrow.
//
// STABILITY: v0.x — UNSTABLE. Per docs/v4-plugin-sdk-9.0-resolution.md §0.3 the
// contract is intentionally NOT frozen until v4 GA: it must first be exercised by
// the Step 10 install-wizard and the gRPC host (internal/extension/grpc)
// consumers and validated against representative driver parity. Until then,
// breaking changes may land on minor version bumps. Do not treat any type here
// as stable.
//
// BOUNDARY RULE: this module MUST NOT import anything from the core's internal/
// tree. It is a standalone Go module (its own go.mod; the root module pulls it in
// through a replace directive) precisely so that out-of-tree extensions can depend
// on the SDK without dragging in the whole core. Keep the dependency set to the Go
// standard library.
package ext
