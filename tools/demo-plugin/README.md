# demo-plugin

A real, buildable `ext.StorageDriver` gRPC plugin used to exercise the full
plugin path (build -> package -> sign -> install -> wizard -> contributions ->
dispatch) end to end. It is deliberately trivial: `List`/`Link`/`Get` all
answer with one fixed stub object (`hello-from-plugin.txt`), so the surface
under test is the plugin machinery, not driver logic.

`driver.go`'s `Manifest()` is kept byte-for-byte equivalent (as JSON) to
`plugin.json` in this directory: the gRPC `GetManifest` RPC reads the former
at runtime, the install gateway reads the latter from the package. They must
never drift.

## Build (Task 1)

```bash
export GOFLAGS=-mod=mod
go build -o tools/demo-plugin/dist/plugin ./tools/demo-plugin
```

This produces a native go-plugin gRPC binary at `tools/demo-plugin/dist/plugin`.

## Package + sign (Task 2)

A plugin package is a directory containing `plugin.json`, the platform
binary(ies), a `checksums.txt` (sha256 of every other file, sorted by relpath),
and a `signature` file (base64 ed25519 signature over `checksums.txt`'s raw
bytes) — see `internal/extension/sign.go` (`ComputeChecksums`, `ChecksumsFile`,
`SignatureFile`).

Expected steps for Task 2:

```bash
# 1. build the binary (see above), then stage a package dir:
mkdir -p tools/demo-plugin/dist/pkg
cp tools/demo-plugin/dist/plugin tools/demo-plugin/dist/pkg/
cp tools/demo-plugin/plugin.json tools/demo-plugin/dist/pkg/

# 2. generate a throwaway ed25519 keypair for local signing
#    (do not commit *.ed25519 — see .gitignore)

# 3. write checksums.txt over the package dir and sign it with the private key,
#    producing dist/pkg/checksums.txt and dist/pkg/signature

# 4. install the package dir through the extension install gateway and confirm:
#    - ValidateManifest passes (core_api "^0.1.0" satisfies the host)
#    - the wizard renders the 4 declared steps (info/form/test/done)
#    - GET /api/v4/extensions/contributions includes this plugin's
#      config_form + menu_actions
#    - List/Link/Get round-trip over gRPC and return the hello-from-plugin.txt stub
```

## Layout

- `driver.go` — the `demoDriver` (`ext.StorageDriver` + `ext.Getter`)
- `main.go` — `grpcplugin.Serve(newDemoDriver())`
- `plugin.json` — the v0.1.0 manifest (id/version/core_api/kinds/capabilities/
  provenance/permissions/wizard/contributions), kept in sync with `Manifest()`
- `dist/` — build output, gitignored
