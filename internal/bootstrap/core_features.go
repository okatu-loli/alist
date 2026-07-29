package bootstrap

import (
	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// InitCoreFeatureExtensions registers AList's natively-wired core features as
// plain, manageable built-in extensions (Project A-2): offline download,
// file search, sharing, SSO login, internal WebDAV, passkey login (design W5),
// and the internal TOTP second factor (Phase C). Each becomes a Kind=Feature registry
// entry so it lists in the extension manager, toggles through
// EnableBuiltin/DisableBuiltin with persistence, and backs a
// RequireExtensionEnabled route gate — without re-plumbing the feature itself
// (its routes/handlers already live in core).
//
// It runs after the other Init* built-in registrations and BEFORE
// InitBuiltinManage so ApplyDisabledBuiltins re-applies an admin's persisted
// "off" choice to these entries too. A registration failure is fatal: a
// half-wired built-in must not run.
func InitCoreFeatureExtensions() {
	features := []ext.Manifest{
		{
			ID:    "builtin.offline_download",
			Name:  map[string]string{"en": "Offline Download", "zh": "离线下载"},
			Kinds: []ext.Kind{ext.KindFeature},
			// Native host settings page (design C3): the /@manage/ prefix marks it a
			// host-rendered page, and the route now points at the generic
			// FeatureConfigPage (/@manage/feature/<id>) instead of the raw
			// OFFLINE_DOWNLOAD SettingList — so the extension manager's link lands
			// on the base feature's config page (general section + each enabled
			// tool's section) rather than a standalone settings group.
			Contributions: ext.Contributions{
				SettingsPages: []ext.SettingsPage{{
					ID:    "offline_download",
					Title: map[string]string{"en": "Offline Download", "zh": "离线下载"},
					Route: "/@manage/feature/builtin.offline_download",
				}},
				// The base feature declares its own "general" config section
				// (design C): the shared, tool-agnostic settings rendered at the
				// top of the offline-download feature config page. Each enabled
				// tool extension appends its own section (Parent=this id).
				ConfigSections: []ext.ConfigSection{{
					Parent: "builtin.offline_download",
					Title:  map[string]string{"en": "General", "zh": "通用配置"},
					SettingKeys: []string{
						"offline_download_tool",
						"offline_download_task_threads_num",
						"offline_download_transfer_threads_num",
						"offline_download_max_size_gb",
					},
					Order: 0,
				}},
			},
		},
		{
			ID:    "builtin.search",
			Name:  map[string]string{"en": "File Search", "zh": "文件搜索"},
			Kinds: []ext.Kind{ext.KindFeature},
			// Native host settings page (design C4): the /@manage/ prefix marks it a
			// host-rendered page, and the route now points at the generic
			// FeatureConfigPage (/@manage/feature/<id>) instead of the raw INDEX
			// SettingList — so the extension manager's link lands on the search base
			// feature's config page (general section + each enabled backend's
			// section) rather than a standalone settings group.
			Contributions: ext.Contributions{
				SettingsPages: []ext.SettingsPage{{
					ID:    "search",
					Title: map[string]string{"en": "File Search", "zh": "文件搜索"},
					Route: "/@manage/feature/builtin.search",
				}},
				// The base feature declares its own "general" config section
				// (design C4): the shared, backend-agnostic settings rendered at the
				// top of the search feature config page. Each enabled backend
				// extension appends its own section (Parent=this id).
				ConfigSections: []ext.ConfigSection{{
					Parent: "builtin.search",
					Title:  map[string]string{"en": "General", "zh": "通用配置"},
					SettingKeys: []string{
						"search_index",
						"auto_update_index",
						"search_max_depth",
						"search_result_limit",
					},
					Order: 0,
				}},
			},
		},
		{
			ID:    "builtin.share",
			Name:  map[string]string{"en": "Sharing", "zh": "分享"},
			Kinds: []ext.Kind{ext.KindFeature},
			DisableWarning: map[string]string{
				"en": "Disabling this immediately invalidates all public share links.",
				"zh": "关闭后所有公开分享链接将立即失效。",
			},
		},
		{
			ID:    "builtin.sso",
			Name:  map[string]string{"en": "SSO Login", "zh": "单点登录"},
			Kinds: []ext.Kind{ext.KindFeature},
			DisableWarning: map[string]string{
				"en": "Disabling this stops SSO login; users who linked an SSO identity must sign in with their password.",
				"zh": "关闭后 SSO 登录将不可用,已绑定 SSO 的用户须改用密码登录。",
			},
			// Declare the native host management page so the merged
			// contributions document surfaces it and the frontend generates the
			// nav entry from data (design W1). The /@manage/ prefix marks it a
			// native host page (App.tsx already routes /extensions/sso→SSOList),
			// not a sandboxed detail-page section.
			Contributions: ext.Contributions{
				SettingsPages: []ext.SettingsPage{{
					ID:    "sso",
					Title: map[string]string{"en": "SSO Login", "zh": "单点登录"},
					Route: "/@manage/extensions/sso",
				}},
			},
		},
		{
			ID:    "builtin.webdav",
			Name:  map[string]string{"en": "WebDAV", "zh": "WebDAV"},
			Kinds: []ext.Kind{ext.KindFeature},
		},
		{
			// Passkey (WebAuthn) login (design W5). The passwordless login method
			// itself is registered SEPARATELY as a trusted in-process AuthNMethod
			// (InitPasskey → passkey.RegisterBuiltin, in the authNInstances map);
			// THIS entry is only the manageable Feature identity in the registry's
			// byID map, so an admin can disable passkey. The two share the id
			// "builtin.passkey" but live in independent maps and never collide:
			// Get() reads this Feature entry, GetAuthNMethod() reads the method
			// instance. Disabling this entry is what takes passkey offline on every
			// path (login dispatch gate in server/handles/authn.go + the
			// /auth/passkey self-management group gate in router_v4.go); the method
			// instance keeps existing, unused, until re-enabled.
			//
			// Kinds ALSO declares KindAuthNMethod (Phase F): this Feature entry is
			// what AggregateContributions/PublicLoginMethods actually read
			// Contributions from (the separately-registered trusted AuthNMethod
			// instance above carries its own, unused-for-registry-purposes
			// ext.Manifest via authNMethod.Manifest()) — and login_methods[] now
			// requires DeclaresKind(AuthNMethod) on the SAME manifest that declares
			// it (ValidateManifest's single-id addressing contract,
			// internal/extension/manifest.go). The primary registry Kind stays
			// Feature (Kinds[0]), so every existing Get()/gate keyed on
			// Kind==Feature is unaffected — this only ADDS a second declared Kind.
			//
			// UIRef is deliberately left empty: the login page does not render
			// passkey through the generic ui_ref iframe path (that's for a
			// plugin's own sandboxed ceremony bundle) — it already special-cases
			// "builtin.passkey" with a dedicated native component (navigator.
			// credentials WebAuthn call), and continues to do so. This entry exists
			// so passkey surfaces in the login_methods AGGREGATE at all (for a
			// frontend that wants to discover-and-branch on presence), not to hand
			// it a UI bundle route.
			ID:    "builtin.passkey",
			Name:  map[string]string{"en": "Passkey", "zh": "通行密钥"},
			Kinds: []ext.Kind{ext.KindFeature, ext.KindAuthNMethod},
			Contributions: ext.Contributions{
				LoginMethods: []ext.LoginMethod{{
					ID:    "builtin.passkey",
					Kind:  "primary",
					Label: map[string]string{"en": "Sign in with a passkey", "zh": "使用通行密钥登录"},
				}},
			},
			DisableWarning: map[string]string{
				"en": "Disabling this turns off passkey (WebAuthn) login; existing passkeys stop working until it is re-enabled — affected users sign in with their password.",
				"zh": "关闭后通行密钥(WebAuthn)登录不可用,已注册的通行密钥在重新启用前失效,相关用户须改用密码登录。",
			},
		},
		{
			// TOTP second factor (Phase C: internal TOTP second factor). Same
			// two-map split as builtin.passkey right above: the trusted
			// AuthNMethod instance is registered SEPARATELY (InitTOTP →
			// totp.RegisterBuiltin, in the authNInstances map); THIS entry is only
			// the manageable Feature identity in the registry's byID map, so an
			// admin can disable TOTP. Disabling it is what makes the login
			// orchestrator's step-up check (server/handles/auth.go) degrade to "no
			// second factor" even for a user who still has OtpSecret set — the
			// self-service bind/unbind/recovery-code routes (gated on this same id
			// in router_v4.go) go 403 too.
			ID:    "builtin.totp",
			Name:  map[string]string{"en": "TOTP Second Factor", "zh": "TOTP 二次验证"},
			Kinds: []ext.Kind{ext.KindFeature},
			DisableWarning: map[string]string{
				"en": "Disabling this turns off the TOTP second factor for EVERY user: login no longer asks for a code, even for accounts that still have one enrolled — re-enabling restores the step-up prompt for them.",
				"zh": "关闭后所有用户的 TOTP 二次验证将不再生效:登录不再要求验证码,即使账户仍绑定了 TOTP——重新启用会恢复其二次验证提示。",
			},
		},
	}
	for _, m := range features {
		m.Version = "builtin"
		m.Provenance = ext.Provenance{Source: "official"}
		if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, m); err != nil {
			utils.Log.Fatalf("failed to register built-in core feature %q: %+v", m.ID, err)
		}
	}
}
