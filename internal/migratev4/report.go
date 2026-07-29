package migratev4

import (
	"fmt"
	"sort"
	"strings"
)

// Report is the per-domain outcome of a migration, shared by --dry-run (where
// it is computed but nothing is written) and a real run (where it describes what
// was written). The two are identical in shape by design so a dry-run preview
// matches the eventual result.
type Report struct {
	DryRun bool

	SettingsCarried int
	SettingsDropped []string // v3 keys with no v4 equivalent

	UsersCarried int
	RolesCarried int // includes builtin roles ensured on the target

	StoragesCarried  int
	StoragesDisabled []string // "mount (driver)" — driver absent from this v4 build

	MetasCarried int

	TasksSkipped int64
	NotCarried   map[string]int64 // dropped plugin-era domain -> source row count

	// BasePathConversions lists users whose v3 base_path was converted into a
	// private RBAC permission scope (v4 has no BasePath). v3 confinement and v4
	// grant semantics are not identical, so the admin should review each.
	BasePathConversions []BasePathConversion
}

// BasePathConversion records that a v3 user's base_path fence was migrated into
// a private "migrated-scope:<username>" permission scope role.
type BasePathConversion struct {
	Username string
	Path     string
}

// AddBasePathConversion appends a base_path->scope conversion to the report.
func (r *Report) AddBasePathConversion(username, path string) {
	r.BasePathConversions = append(r.BasePathConversions, BasePathConversion{Username: username, Path: path})
}

// standing v4 behaviour changes that hold regardless of the source data. These
// are the breaking changes a v3 admin must know about; they are reprinted with
// every report (dry-run and real) so the preview is a complete briefing.
var standingChanges = []string{
	"Old signed download links (/d, /p) become invalid — v4 uses a new signing keyset.",
	"All existing sessions and API tokens are reset — every user must log in again.",
	"The v3 API is deprecated — update any integrations to the v4 API.",
}

func (r *Report) String() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("=== alist migrate v4 — DRY RUN (no data written) ===\n")
	} else {
		b.WriteString("=== alist migrate v4 — migration complete ===\n")
	}

	fmt.Fprintf(&b, "settings: %d carried", r.SettingsCarried)
	if len(r.SettingsDropped) > 0 {
		dropped := append([]string(nil), r.SettingsDropped...)
		sort.Strings(dropped)
		fmt.Fprintf(&b, ", %d dropped (no v4 equivalent): %s", len(dropped), strings.Join(dropped, ", "))
	}
	b.WriteByte('\n')

	fmt.Fprintf(&b, "roles: %d carried (incl. builtin)\n", r.RolesCarried)
	fmt.Fprintf(&b, "users: %d carried (passwords kept; roles rebound; sessions reset)\n", r.UsersCarried)

	fmt.Fprintf(&b, "storages: %d carried", r.StoragesCarried)
	if len(r.StoragesDisabled) > 0 {
		disabled := append([]string(nil), r.StoragesDisabled...)
		sort.Strings(disabled)
		fmt.Fprintf(&b, ", %d imported DISABLED (driver not in this v4 build): %s",
			len(disabled), strings.Join(disabled, ", "))
	}
	b.WriteByte('\n')

	fmt.Fprintf(&b, "metas: %d carried (path anchors resolve on first storage load)\n", r.MetasCarried)
	fmt.Fprintf(&b, "tasks: %d skipped (transient, not carried)\n", r.TasksSkipped)

	if len(r.NotCarried) > 0 {
		labels := make([]string, 0, len(r.NotCarried))
		for k := range r.NotCarried {
			labels = append(labels, k)
		}
		sort.Strings(labels)
		b.WriteString("not carried into v4 (plugin-era domains, left untouched in source):\n")
		for _, k := range labels {
			fmt.Fprintf(&b, "  - %s: %d row(s)\n", k, r.NotCarried[k])
		}
	}

	if len(r.BasePathConversions) > 0 {
		conv := append([]BasePathConversion(nil), r.BasePathConversions...)
		sort.Slice(conv, func(i, j int) bool { return conv[i].Username < conv[j].Username })
		b.WriteString("v3 base_path converted to permission scope (REVIEW — confinement vs grant semantics differ):\n")
		for _, c := range conv {
			fmt.Fprintf(&b, "  - %s -> %s\n", c.Username, c.Path)
		}
	}

	b.WriteString("standing v4 behaviour changes:\n")
	for _, c := range standingChanges {
		fmt.Fprintf(&b, "  - %s\n", c)
	}
	return b.String()
}
