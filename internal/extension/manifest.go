package extension

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/AlistGo/alist-ext"
)

// ManifestFile is the manifest filename inside a plugin package/dir (SDK §2).
const ManifestFile = "plugin.json"

// pluginIDRe constrains a plugin id to a filesystem- and namespace-safe shape:
// it starts with an alphanumeric and thereafter allows only alphanumerics, dot,
// hyphen, and underscore. The plugin id becomes a path segment for on-disk
// install/state/KV/Secrets/Table storage (e.g. <DataDir>/plugins/<id>/…), so an
// id carrying a path separator, drive/scheme colon, a ".." sequence, or a
// leading dot (which would collide with dotfiles such as .secrets/.secrets.key)
// must be refused at the trust boundary — not sanitized downstream. Real ids
// (builtin.webdav, cn.alist.123open, com.example.demo) all satisfy this.
var pluginIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// publicPrefixRe constrains Contributions.PublicPrefix to a single, rooted,
// lowercase alphanumeric-plus-hyphen segment (e.g. "/s"): the shape a
// PublicPage route like "/s/:share_id" is mounted under (SDK doc §3
// PublicRouter, ext/contributions.go). This is a FORMAT check only — the
// reserved-word conflict (CheckReservedPrefix) and cross-plugin mutual
// exclusion run separately at registration (registry.go
// claimPublicPrefixLocked), where the Registry's live claim table is
// available; ValidateManifest has neither the registry nor the plugin id
// namespace context to check either.
var publicPrefixRe = regexp.MustCompile(`^/[a-z0-9-]+$`)

// validatePublicPrefixFormat checks the shape of a requested public prefix,
// returning a descriptive error for a malformed one (missing leading slash,
// more than one segment, uppercase, or any character outside [a-z0-9-]).
func validatePublicPrefixFormat(prefix string) error {
	if !publicPrefixRe.MatchString(prefix) {
		return fmt.Errorf("public_prefix %q must be a single rooted lowercase alphanumeric/hyphen segment (e.g. \"/s\")", prefix)
	}
	return nil
}

// knownKinds is the frozen set of extension-point identifiers a manifest may
// declare (mirrors ext.Kind constants). A manifest naming an unknown Kind is
// rejected: an unknown Kind means the host cannot place the extension.
var knownKinds = map[ext.Kind]struct{}{
	ext.KindStorageDriver: {}, ext.KindAuthProvider: {}, ext.KindAuthNMethod: {},
	ext.KindOfflineDownloadTool: {}, ext.KindArchiveTool: {}, ext.KindSearchIndexer: {},
	ext.KindThumbnailer: {}, ext.KindFileTransformer: {}, ext.KindMetadataProvider: {},
	ext.KindEventHook: {}, ext.KindScheduledJob: {}, ext.KindNotification: {},
	ext.KindAction: {}, ext.KindPreviewer: {}, ext.KindUIPanel: {},
	ext.KindAPIRoute: {}, ext.KindAccessProtocol: {}, ext.KindFeature: {},
	ext.KindBackgroundService: {}, ext.KindFeatureBundle: {}, ext.KindFileDecorator: {},
}

// LoadManifest reads and validates plugin.json from a plugin directory (an
// installed <DataDir>/plugins/<id>/<version>/ or a staged package dir).
func LoadManifest(dir string) (ext.Manifest, error) {
	var m ext.Manifest
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return m, fmt.Errorf("extension: read manifest: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("extension: parse manifest: %w", err)
	}
	if err := ValidateManifest(m); err != nil {
		return m, err
	}
	return m, nil
}

// ValidateManifest checks required fields, that every declared Kind is known,
// and that the plugin's core_api constraint is compatible with the host
// CoreAPIVersion. It is the single gate every manifest passes before the host
// will place a plugin.
func ValidateManifest(m ext.Manifest) error {
	if m.ID == "" {
		return fmt.Errorf("extension: manifest missing id")
	}
	if !pluginIDRe.MatchString(m.ID) || strings.Contains(m.ID, "..") {
		return fmt.Errorf("extension %q: invalid id (must be alphanumeric plus . _ -, no path separators or ..)", m.ID)
	}
	if m.Version == "" {
		return fmt.Errorf("extension %q: manifest missing version", m.ID)
	}
	if _, err := parseSemver(m.Version); err != nil {
		return fmt.Errorf("extension %q: invalid version: %w", m.ID, err)
	}
	if len(m.Kinds) == 0 {
		return fmt.Errorf("extension %q: manifest declares no kinds", m.ID)
	}
	for _, k := range m.Kinds {
		if _, ok := knownKinds[k]; !ok {
			return fmt.Errorf("extension %q: unknown kind %q", m.ID, k)
		}
	}
	if m.CoreAPI == "" {
		return fmt.Errorf("extension %q: manifest missing core_api", m.ID)
	}
	ok, err := satisfies(CoreAPIVersion, m.CoreAPI)
	if err != nil {
		return fmt.Errorf("extension %q: core_api %q: %w", m.ID, m.CoreAPI, err)
	}
	if !ok {
		return fmt.Errorf("extension %q: core_api %q incompatible with host core API %s",
			m.ID, m.CoreAPI, CoreAPIVersion)
	}
	if p := m.Contributions.PublicPrefix; p != "" {
		if err := validatePublicPrefixFormat(p); err != nil {
			return fmt.Errorf("extension %q: %w", m.ID, err)
		}
	}
	if err := validateLoginMethodsAddressing(m); err != nil {
		return err
	}
	return nil
}

// validateLoginMethodsAddressing enforces the single-id AuthNMethod addressing
// contract decided for design's login-flow work (Phase F, ext/authn_method.go's
// AuthMethodInfo.ID doc): an extension that declares login_methods in its
// Contributions must ALSO declare Kind=AuthNMethod (hasKind, the Manifest truth
// source — mirrors DeclaresKind's rule), and every declared login_methods[].ID
// must equal the manifest's own ID. This is what makes a contributed login
// method always reachable at /auth/method/<manifest-id>/... with no separate
// per-method id to resolve. The built-in SSO bridge is NOT checked here because
// it is exempt by construction: its per-provider "sso:<key>" methods are
// registered directly via extension.RegisterTrustedAuthNMethodInstance and never
// carry a real Manifest through ValidateManifest at all.
func validateLoginMethodsAddressing(m ext.Manifest) error {
	if len(m.Contributions.LoginMethods) == 0 {
		return nil
	}
	if !hasKind(m, ext.KindAuthNMethod) {
		return fmt.Errorf("extension %q: declares login_methods but does not declare kind %q", m.ID, ext.KindAuthNMethod)
	}
	for _, lm := range m.Contributions.LoginMethods {
		if lm.ID != m.ID {
			return fmt.Errorf("extension %q: login_methods[].id must equal the extension's own manifest id (single-id addressing contract); got %q", m.ID, lm.ID)
		}
	}
	return nil
}
