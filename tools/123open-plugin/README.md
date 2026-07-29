# 123open-plugin

The 123 Open storage driver, packaged as an installable `ext.StorageDriver`
gRPC plugin. It is the real (non-demo) counterpart to `tools/demo-plugin`: the
driver logic is the unmodified `drivers/123_open.Open123` (`Init`/`List`/
`Link`/`Mkdir`/`Move`/`Rename`/`Remove` — all plain HTTP against the 123 open
platform API), wrapped by `internal/extension/bridge.StorageBridge` and served
over `internal/extension/grpcplugin`.

123 Open is **no longer built into the alist binary**: `drivers/all.go` does
not blank-import `drivers/123_open` any more, so the driver is only available
by installing this plugin. The driver package itself is untouched (including
its own `driver_test.go`) — only its in-process registration was removed.

`driver.go`'s `manifest()` is kept semantically equivalent (as decoded JSON)
to `plugin.json` in this directory: the gRPC `GetManifest` RPC reads the
former at runtime, the install gateway reads the latter from the packaged
archive. `driver_test.go` (`TestManifestMatchesPluginJSON`) guards against
drift by decoding both and comparing with `reflect.DeepEqual`.

## Design notes

- **Capabilities are hand-trimmed, not derived — treat `driver.go`'s
  `manifest()` as the source of truth, not this list.** `Open123` implements
  `Copy`/`GetArchiveMeta`/`ListArchive`/`Extract`/`ArchiveDecompress` only
  because `driver.Driver` requires the methods to exist — every one of them
  returns `errs.NotSupport`. An automatic capability probe would falsely
  advertise them, so the manifest omits them. `Put` (upload) and `PutRapid`
  (hash-based "秒传" rapid upload — the gRPC bridge reaches straight through
  to `drivers/123_open`'s `create(etag)` call, no bytes transferred on a hit)
  are both wired and declared.
- **The install wizard does not collect credentials.** `client_id`/
  `client_secret`/etc. belong to each storage instance (added later, one per
  123 open platform account), not to installing the driver once. The wizard
  is just `info` (what this installs) → `done`.
- **`Info` declares the "add storage" form.** `ext.Manifest.Info.Additional`
  mirrors `drivers/123_open/meta.go`'s `Addition` struct field-for-field
  (`client_id`, `client_secret`, `private_key`, `uid`, `valid_duration`,
  `root_folder_id`), with `client_secret`/`private_key` typed `"password"` so
  the frontend masks them. The host synthesizes the generic common fields
  (`mount_path`, `order`, ...) itself — see
  `internal/extension.BuildPluginDriverInfo`.

## Build

```bash
export GOFLAGS=-mod=mod
go build -o tools/123open-plugin/dist/plugin ./tools/123open-plugin
```

This produces a native go-plugin gRPC binary at `tools/123open-plugin/dist/plugin`.

## Package + sign

A plugin package is a directory containing `plugin.json`, the platform
binary(ies), a `checksums.txt` (sha256 of every other file, sorted by relpath),
and a `signature` file (base64 ed25519 signature over `checksums.txt`'s raw
bytes) — see `internal/extension/sign.go` (`ComputeChecksums`, `ChecksumsFile`,
`SignatureFile`).

`pack.sh` does this end to end (build, genkey, sign, pack, write
`dist/index.json`); `serve.sh` then serves `dist/` as a static local plugin
source on `http://127.0.0.1:8766/` (port 8766, distinct from demo-plugin's
8765, so both sources can run at once):

```bash
./tools/123open-plugin/pack.sh
./tools/123open-plugin/serve.sh &
# import http://127.0.0.1:8766/index.json as a plugin source, trusting
# dist/pubkey.hex, then install cn.alist.123open through the usual
# extension install gateway.
```

After installing:

- `ValidateManifest` passes (`core_api` `"^0.1.0"` satisfies the host).
- The wizard renders the 2 declared steps (info/done) — no credential form.
- `GET /api/v4/admin/driver/info` (or the "add storage" dialog) shows a
  `123 Open` entry with `client_id`/`client_secret`/`private_key`/`uid`/
  `valid_duration`/`root_folder_id` fields plus the generic common fields.
- Adding a storage with real 123 open platform credentials calls
  `Open123.Init`, which fetches a token — wrong credentials surface as the
  storage failing to mount, exactly as they did when the driver was built in.

## Layout

- `driver.go` — `manifest()` (the hand-written `ext.Manifest`) and
  `newBridge()` (`bridge.NewStorageBridge` wrapping `*open123.Open123`)
- `main.go` — `grpcplugin.Serve(newBridge())`. No sqlite/gorm bootstrap:
  unlike `internal/extension/grpc/testdata/localplugin` (which serves the
  `local` driver and needs a DB for a settings lookup), `Open123.Init` is pure
  HTTP and touches no host DB.
- `plugin.json` — the v0.1.0 manifest (id/version/core_api/kinds/
  capabilities/provenance/permissions/wizard/info), kept in sync with
  `manifest()`
- `driver_test.go` — `TestManifestMatchesPluginJSON`, the manifest/plugin.json
  drift guard
- `dist/` — build output, gitignored
