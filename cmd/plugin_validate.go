package cmd

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/spf13/cobra"
)

var pluginValidateCmd = &cobra.Command{
	Use:   "validate --dir <plugin-pkg-dir> [--strict]",
	Short: "Offline health check for a plugin package: manifest, binary, signature tier, i18n",
	Run: func(cmd *cobra.Command, args []string) {
		dir, _ := cmd.Flags().GetString("dir")
		strict, _ := cmd.Flags().GetBool("strict")
		pubkeys, _ := cmd.Flags().GetStringArray("pubkey")

		opts := pluginValidateOptions{Dir: dir, Strict: strict, PubKeys: pubkeys}
		ok, err := runPluginValidate(opts, os.Stdout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "plugin validate:", err)
			os.Exit(1)
		}
		if !ok {
			os.Exit(1)
		}
	},
}

func init() {
	pluginCmd.AddCommand(pluginValidateCmd)

	pluginValidateCmd.Flags().String("dir", "", "plugin package directory containing plugin.json [required]")
	pluginValidateCmd.Flags().Bool("strict", false, "treat an unsigned/unverified (third-party tier) package as an error, not a warning")
	pluginValidateCmd.Flags().StringArray("pubkey", nil, "ed25519 public key hex (repeatable) to additionally trust — e.g. your own plugin source signing key")
}

// pluginValidateOptions are the resolved `alist plugin validate` inputs,
// independent of cobra flag parsing so runPluginValidate is directly
// unit-testable.
type pluginValidateOptions struct {
	Dir     string
	Strict  bool
	PubKeys []string // extra ed25519 public keys (hex) to trust, e.g. an author's own source key
}

// runPluginValidate runs an offline health check over a plugin package
// directory: manifest, binary/artifacts, signature trust tier, and bundled
// i18n. Unlike the install-time gates it never fails fast — each check runs
// independently and reports what it found, so a single problem doesn't hide
// the rest. report receives one "[ok]/[warn]/[error] <check>: <detail>" line
// per finding (the cobra Run func passes os.Stdout; tests pass a buffer). ok
// is false iff at least one [error] line was emitted.
func runPluginValidate(opts pluginValidateOptions, report io.Writer) (ok bool, err error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return false, fmt.Errorf("--dir is required")
	}
	ok = true

	man := validatePluginManifest(dir, report, &ok)
	validatePluginBinary(dir, report, &ok)
	validatePluginPubkeys(opts.PubKeys, report, &ok)
	validatePluginSignature(dir, man, opts.Strict, opts.PubKeys, report, &ok)
	validatePluginI18n(dir, man, report, &ok)

	return ok, nil
}

// validatePluginPubkeys checks that every --pubkey value is a well-formed
// ed25519 public key (64 hex chars decoding to ed25519.PublicKeySize bytes).
// Without this, a typo'd or truncated --pubkey silently does nothing: it
// flows into offlineTieredVerifier as an ad-hoc PluginSource, and
// extension.TieredVerifier.DetermineTier skips any source whose Key() fails
// to parse ("a malformed source key never grants trust") — so the package
// just validates at whatever tier it would have anyway (typically the
// unrecognized-signature hard error), with no indication that the flag
// itself was the problem. This check reports it explicitly instead.
func validatePluginPubkeys(pubkeys []string, report io.Writer, ok *bool) {
	for _, k := range pubkeys {
		raw, err := hex.DecodeString(strings.TrimSpace(k))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			*ok = false
			fmt.Fprintf(report, "[error] pubkey: %s 不是合法的 ed25519 公钥 hex\n", k)
		}
	}
}

// validatePluginManifest runs extension.LoadManifest's strict validation
// first. On failure it reports the error and falls back to the lenient
// readManifest (id presence only) so the remaining, independent checks below
// can still run against whatever plugin.json declares — validate's job is to
// report as much as it can, not to stop at the first problem.
func validatePluginManifest(dir string, report io.Writer, ok *bool) ext.Manifest {
	man, err := extension.LoadManifest(dir)
	if err == nil {
		fmt.Fprintf(report, "[ok] manifest: ok (id=%s, version=%s)\n", man.ID, man.Version)
		return man
	}
	*ok = false
	fmt.Fprintf(report, "[error] manifest: %v\n", err)

	lenient, lerr := readManifest(dir)
	if lerr != nil {
		fmt.Fprintf(report, "[error] manifest: fallback parse also failed: %v\n", lerr)
		return man // zero-value manifest; later checks do what they can with it
	}
	return lenient
}

// validatePluginBinary checks that the package ships exactly one kind of
// runnable artifact (native or WASM, never both, never neither) and, when
// exactly one is present, that findBinary can actually resolve it.
func validatePluginBinary(dir string, report io.Writer, ok *bool) {
	hasNative, hasWASM := extension.DetectArtifacts(dir)
	switch {
	case hasNative && hasWASM:
		*ok = false
		fmt.Fprintf(report, "[error] binary: package ships both a native binary and a .wasm module; expected exactly one\n")
	case !hasNative && !hasWASM:
		*ok = false
		fmt.Fprintf(report, "[error] binary: no runnable binary found in %s\n", dir)
	default:
		bin, err := extension.FindBinary(dir)
		if err != nil {
			*ok = false
			fmt.Fprintf(report, "[error] binary: %v\n", err)
			return
		}
		fmt.Fprintf(report, "[ok] binary: %s\n", bin)
	}
}

// cliPubKeySourceName tags the pseudo plugin-source the --pubkey flag builds,
// so a validate report distinguishes "verified against a CLI-supplied key"
// from an admin-imported source name.
const cliPubKeySourceName = "cli:--pubkey"

// offlineTieredVerifier builds a TieredVerifier scoped to validate's offline
// contract: it checks a package's signature against the embedded official
// trust root, plus any extra keys the caller passed via --pubkey. It
// deliberately does NOT use extension.DefaultTieredVerifier — that wires in
// extension.CurrentSources, which reads the plugin_sources admin setting
// through internal/op/internal/db, and panics when no DB has been opened.
// `alist plugin validate` is a standalone dev command (no `server`
// bootstrap, no --data DB), so it must never touch the DB. Instead,
// --pubkey-supplied keys are threaded through NewTieredVerifier's sources
// callback as ad-hoc PluginSource entries (same mechanism an admin-imported
// source uses) — the practical effect: a package a plugin author signed with
// their own source key verifies here as long as that key is passed via
// --pubkey; without it (and without an official signature), it is a known
// limitation: the signature is present but this offline check has no way to
// recognize it, so it hard-fails (with a hint to use --pubkey).
func offlineTieredVerifier(pubkeys []string) *extension.TieredVerifier {
	raw, err := hex.DecodeString(extension.OfficialPublicKeyHex())
	if err != nil || len(raw) != ed25519.PublicKeySize {
		// The embedded constant is a build-time invariant (mirrors
		// extension.mustDecodeKey, which already panics on this same string
		// at package init if it were ever malformed).
		panic("plugin validate: malformed embedded official public key")
	}

	var sourcesFn func() []extension.PluginSource
	if len(pubkeys) > 0 {
		srcs := make([]extension.PluginSource, 0, len(pubkeys))
		for _, k := range pubkeys {
			srcs = append(srcs, extension.PluginSource{
				Name:   cliPubKeySourceName,
				PubKey: strings.TrimSpace(k),
			})
		}
		sourcesFn = func() []extension.PluginSource { return srcs }
	}
	return extension.NewTieredVerifier(ed25519.PublicKey(raw), sourcesFn)
}

// validatePluginSignature determines the package's trust tier. A present but
// unverifiable signature (tampered contents, or signed by a key neither the
// official root nor any --pubkey recognizes) is DetermineTier's
// *SignatureError hard-failure path and always an error — the report hints
// at --pubkey, since this is exactly the shape a community plugin author's
// own-key-signed (but not tampered) package takes when its key isn't
// supplied. A cleanly unsigned (or closed/paid-forced) package is the
// legitimate third-party tier: a warning by default, escalated to an error
// under --strict.
func validatePluginSignature(dir string, man ext.Manifest, strict bool, pubkeys []string, report io.Writer, ok *bool) {
	tier, source, err := offlineTieredVerifier(pubkeys).DetermineTier(dir, man)
	if err != nil {
		*ok = false
		fmt.Fprintf(report, "[error] signature: %v; 若这是你自己源密钥签的包,用 --pubkey 提供该公钥\n", err)
		return
	}

	label := "tier: " + tier.String()
	if source != "" {
		label += fmt.Sprintf(" (source: %s)", source)
	}
	if tier != extension.TierThirdParty {
		fmt.Fprintf(report, "[ok] %s\n", label)
		return
	}

	// A closed-source and/or paid manifest (extension.ClosedOrPaid) is forced
	// into TierThirdParty regardless of signature validity — even a
	// perfectly valid official/source signature can't buy back auditability
	// (see trust.go's DetermineTier). Reporting that identically to a merely
	// unsigned package would mislead the author into thinking a valid
	// signature is all that's missing, when the manifest itself
	// (license/source_url/pricing) is why it can never rise above
	// third-party.
	reason := "unsigned or unverified"
	if extension.ClosedOrPaid(man) {
		reason = "third-party(闭源/付费包,即使签名有效也按第三方处理)"
	}
	if strict {
		*ok = false
		fmt.Fprintf(report, "[error] %s (%s); --strict requires an officially or source-verified signed package\n", label, reason)
		return
	}
	fmt.Fprintf(report, "[warn] %s (%s)\n", label, reason)
}

// validatePluginI18n walks dir/i18n/*.json. Every file must parse as a flat
// string map — production's LoadPluginLocales silently skips a malformed
// file, which is exactly the failure mode validate exists to catch, so a
// parse failure here is always an error. The "en" locale is additionally
// checked against expectedKeys(man): missing keys are an error, stale extra
// keys a warning. A package with config fields (Info.Additional) but no
// i18n/ directory at all gets a warning, not an error — bundled locales are
// opt-in.
func validatePluginI18n(dir string, man ext.Manifest, report io.Writer, ok *bool) {
	i18nDir := filepath.Join(dir, "i18n")
	entries, err := os.ReadDir(i18nDir)
	if err != nil {
		if !os.IsNotExist(err) {
			*ok = false
			fmt.Fprintf(report, "[error] i18n: reading %s: %v\n", i18nDir, err)
			return
		}
		if n := len(expectedKeys(man)); n > 0 {
			fmt.Fprintf(report, "[warn] i18n: no i18n/ directory found, but manifest declares %d config field(s) needing labels\n", len(man.Info.Additional))
		} else {
			fmt.Fprintf(report, "[ok] i18n: no i18n/ directory (no config fields declared)\n")
		}
		return
	}

	expected := expectedKeys(man)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		lang := strings.TrimSuffix(e.Name(), ".json")
		path := filepath.Join(i18nDir, e.Name())

		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			*ok = false
			fmt.Fprintf(report, "[error] i18n: %s: %v\n", e.Name(), rerr)
			continue
		}
		var m map[string]string
		if jerr := json.Unmarshal(raw, &m); jerr != nil {
			*ok = false
			fmt.Fprintf(report, "[error] i18n: %s: not a flat string map (production LoadPluginLocales silently skips this file): %v\n", e.Name(), jerr)
			continue
		}

		if lang != "en" {
			fmt.Fprintf(report, "[ok] i18n: %s: valid JSON\n", e.Name())
			continue
		}
		miss := missingKeys(m, expected)
		extra := extraKeys(m, expected)
		if len(miss) > 0 {
			*ok = false
			fmt.Fprintf(report, "[error] i18n: en.json missing %d key(s): %s\n", len(miss), strings.Join(miss, ", "))
		}
		if len(extra) > 0 {
			fmt.Fprintf(report, "[warn] i18n: en.json has %d extra key(s) (no longer in manifest): %s\n", len(extra), strings.Join(extra, ", "))
		}
		if len(miss) == 0 && len(extra) == 0 {
			fmt.Fprintf(report, "[ok] i18n: en.json: %d key(s), matches manifest\n", len(expected))
		}
	}
}
