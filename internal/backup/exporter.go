package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// Options controls what an export contains. Sanitize and a non-empty Password
// are mutually exclusive: sanitizing already removes the secrets that
// encryption would protect, and combining them would imply protecting data
// that is no longer there.
//
// IncludeSecretsKey additionally carries the shared secrets key (G4 §4 — the
// same key internal/op's storage Addition encryption and internal/extension's
// plugin config_form encryption use) inside the archive, so a restore onto a
// different machine (or a wiped data dir) can still decrypt every
// enc:v1:-prefixed value the archive's own Storages/plugin state carries —
// without it, those credentials are permanently unrecoverable after such a
// restore, not merely re-enterable. It requires Password: the key is only
// ever written into the archive's AES-256-GCM-encrypted section, never in
// the plaintext manifest or an unencrypted member, so an unencrypted export
// cannot carry it at all.
type Options struct {
	Password          string
	Sanitize          bool
	IncludeSecretsKey bool
}

// minIncludeSecretsKeyPasswordLen is the floor Options.validate() enforces
// on Password when IncludeSecretsKey is set (Minor 7, G4 red team). Carrying
// the actual secrets key raises the stakes of brute-forcing the archive's
// password well above a normal (keyless) export: without the key, cracking
// the archive password only exposes the OUTER encryption layer — every
// Storage/plugin secret inside is still separately sealed under the
// (unrecovered) secrets key. WITH the key included, cracking the same
// password hands the attacker that key directly, letting them decrypt every
// enc:v1: value in the archive (and, if they can reach it, the live
// database too, since it is the identical shared key). No floor is imposed
// otherwise — a plain encrypted export without the key doesn't share this
// escalation.
const minIncludeSecretsKeyPasswordLen = 12

func (o Options) validate() error {
	if o.Sanitize && o.Password != "" {
		return errors.New("backup: sanitize and password are mutually exclusive")
	}
	if o.IncludeSecretsKey && o.Password == "" {
		return errors.New("backup: including the secrets key requires a password (an unencrypted archive cannot carry it without writing it to disk in plaintext)")
	}
	if o.IncludeSecretsKey && len(o.Password) < minIncludeSecretsKeyPasswordLen {
		return fmt.Errorf("backup: including the secrets key requires a password of at least %d characters", minIncludeSecretsKeyPasswordLen)
	}
	return nil
}

// Dir is the directory backups live in: <DataDir>/backup, alongside the other
// bootstrap paths.
func Dir() string {
	return filepath.Join(flags.DataDir, "backup")
}

// Export collects the current database state and writes a tar.gz into Dir(),
// returning the file name (not full path) and the manifest. The file is named
// backup-<RFC3339-ish timestamp>.tar.gz.
func Export(opts Options) (string, Manifest, error) {
	name := fmt.Sprintf("backup-%s.tar.gz", time.Now().Format("20060102-150405"))
	return exportAs(name, opts)
}

// exportAs writes the export to Dir()/name. Shared by Export and the pre-restore
// snapshot, which needs a caller-chosen name.
func exportAs(name string, opts Options) (string, Manifest, error) {
	if err := opts.validate(); err != nil {
		return "", Manifest{}, err
	}
	arc, err := Collect(db.GetDb())
	if err != nil {
		return "", Manifest{}, err
	}
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		return "", Manifest{}, errors.Wrap(err, "backup: creating backup dir")
	}
	path := filepath.Join(Dir(), name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", Manifest{}, errors.Wrap(err, "backup: creating archive file")
	}
	manifest, werr := Write(f, arc, opts)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(path)
		return "", Manifest{}, werr
	}
	if cerr != nil {
		_ = os.Remove(path)
		return "", Manifest{}, errors.Wrap(cerr, "backup: closing archive file")
	}
	return name, manifest, nil
}

// Collect reads every exported domain from tx into an Archive. It uses the
// shared handle (or a transaction) so a snapshot can be taken inside the same
// transaction as a restore.
func Collect(tx *gorm.DB) (*Archive, error) {
	arc := &Archive{}
	if err := tx.Find(&arc.Settings).Error; err != nil {
		return nil, errors.Wrap(err, "backup: reading settings")
	}
	var users []model.User
	if err := tx.Find(&users).Error; err != nil {
		return nil, errors.Wrap(err, "backup: reading users")
	}
	arc.Users = make([]userRecord, len(users))
	for i, u := range users {
		arc.Users[i] = userToRecord(u)
	}
	if err := tx.Find(&arc.Roles).Error; err != nil {
		return nil, errors.Wrap(err, "backup: reading roles")
	}
	if err := tx.Find(&arc.Permissions).Error; err != nil {
		return nil, errors.Wrap(err, "backup: reading permissions")
	}
	if err := tx.Find(&arc.Metas).Error; err != nil {
		return nil, errors.Wrap(err, "backup: reading metas")
	}
	if err := tx.Find(&arc.Storages).Error; err != nil {
		return nil, errors.Wrap(err, "backup: reading storages")
	}

	// Bindings: read the join rows and translate ids -> names so they survive
	// the id shifts a restore into another instance would cause.
	userNameByID := map[uint]string{}
	for _, u := range users {
		userNameByID[u.ID] = u.Username
	}
	roleNameByID := map[uint]string{}
	for _, r := range arc.Roles {
		roleNameByID[r.ID] = r.Name
	}
	permNameByID := map[uint]string{}
	for _, p := range arc.Permissions {
		permNameByID[p.ID] = p.Name
	}
	var urs []model.UserRole
	if err := tx.Find(&urs).Error; err != nil {
		return nil, errors.Wrap(err, "backup: reading user_roles")
	}
	for _, ur := range urs {
		un, rn := userNameByID[ur.UserID], roleNameByID[ur.RoleID]
		if un == "" || rn == "" {
			continue // dangling binding: drop it rather than export a name that resolves to nothing
		}
		arc.UserRoles = append(arc.UserRoles, userRoleBinding{Username: un, RoleName: rn})
	}
	var rps []model.RolePermission
	if err := tx.Find(&rps).Error; err != nil {
		return nil, errors.Wrap(err, "backup: reading role_permissions")
	}
	for _, rp := range rps {
		rn, pn := roleNameByID[rp.RoleID], permNameByID[rp.PermissionID]
		if rn == "" || pn == "" {
			continue
		}
		arc.RolePermissions = append(arc.RolePermissions, rolePermissionBinding{RoleName: rn, PermissionName: pn})
	}

	portableizeAnchors(arc)
	return arc, nil
}

// portableizeAnchors rewrites node-anchored permission/meta rows into the
// path-carrying, nodeless shape a v3→v4 migration produces, because the nodes
// table is NOT exported: node ids are database-local and would dangle in another
// instance. Each anchored row instead records its LIVE virtual path (derived by
// walking the nodes table now) and clears node_id. On restore this row lands as
// an unadopted path-anchored row, and the first load of its storage runs
// op.adoptNodelessAnchors, which rebuilds the node chain from that path and
// backfills node_id — the anchor comes back ALIVE, so decisions match and
// moves/renames follow again, not merely the row surviving.
//
// A row whose storage was already deleted derives no path (spec §7, it was inert
// in decisions); node_id is still cleared so it never exports as a dangling id
// that could collide with an unrelated node in the target, and it restores as an
// inert nodeless row flagged for manual cleanup, exactly as in the source.
func portableizeAnchors(arc *Archive) {
	for i := range arc.Permissions {
		p := &arc.Permissions[i]
		if p.NodeID == 0 {
			continue
		}
		if vp, err := op.NodeVirtualPath(p.NodeID); err == nil {
			p.Path = vp
		}
		p.NodeID = 0
	}
	for i := range arc.Metas {
		m := &arc.Metas[i]
		if m.NodeID == 0 {
			continue
		}
		if vp, err := op.NodeVirtualPath(m.NodeID); err == nil {
			m.Path = vp
		}
		m.NodeID = 0
	}
}

// Write serializes arc into a tar.gz on w, applying sanitize/encryption per
// opts, and returns the manifest it wrote.
func Write(w io.Writer, arc *Archive, opts Options) (Manifest, error) {
	if err := opts.validate(); err != nil {
		return Manifest{}, err
	}
	if opts.Sanitize {
		sanitize(arc)
	}

	manifest := Manifest{
		FormatVersion: FormatVersion,
		SchemaVersion: migration.LatestID(),
		CreatedAt:     time.Now().UTC(),
		Domains:       map[string]int{},
		Encrypted:     opts.Password != "",
		Sanitized:     opts.Sanitize,
	}

	// Serialize every domain to JSON up front so the manifest's row counts are
	// exact and (when encrypting) we derive the key once.
	payloads := map[string][]byte{
		DomainSettings:        mustJSON(arc.Settings),
		DomainUsers:           mustJSON(arc.Users),
		DomainRoles:           mustJSON(arc.Roles),
		DomainPermissions:     mustJSON(arc.Permissions),
		DomainMetas:           mustJSON(arc.Metas),
		DomainStorages:        mustJSON(arc.Storages),
		DomainUserRoles:       mustJSON(arc.UserRoles),
		DomainRolePermissions: mustJSON(arc.RolePermissions),
	}
	manifest.Domains[DomainSettings] = len(arc.Settings)
	manifest.Domains[DomainUsers] = len(arc.Users)
	manifest.Domains[DomainRoles] = len(arc.Roles)
	manifest.Domains[DomainPermissions] = len(arc.Permissions)
	manifest.Domains[DomainMetas] = len(arc.Metas)
	manifest.Domains[DomainStorages] = len(arc.Storages)
	manifest.Domains[DomainUserRoles] = len(arc.UserRoles)
	manifest.Domains[DomainRolePermissions] = len(arc.RolePermissions)

	var key []byte
	if opts.Password != "" {
		kp, err := newKDFParams()
		if err != nil {
			return Manifest{}, err
		}
		manifest.KDF = &kp
		if key, err = deriveKey(opts.Password, kp); err != nil {
			return Manifest{}, err
		}
	}

	var secretsKeyMember []byte
	if opts.IncludeSecretsKey {
		sk, err := resolveSecretsKey()
		if err != nil {
			return Manifest{}, err
		}
		if secretsKeyMember, err = encrypt(key, sk); err != nil {
			return Manifest{}, err
		}
		manifest.HasSecretsKey = true
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	// manifest.json first, always plaintext.
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, errors.Wrap(err, "backup: marshaling manifest")
	}
	if err := writeTarFile(tw, "manifest.json", manifestBytes); err != nil {
		return Manifest{}, err
	}

	for _, name := range importDomainOrder {
		data := payloads[name]
		if key != nil {
			if data, err = encrypt(key, data); err != nil {
				return Manifest{}, err
			}
		}
		if err := writeTarFile(tw, name+".json", data); err != nil {
			return Manifest{}, err
		}
	}

	if secretsKeyMember != nil {
		if err := writeTarFile(tw, secretsKeyTarName, secretsKeyMember); err != nil {
			return Manifest{}, err
		}
	}

	if err := tw.Close(); err != nil {
		return Manifest{}, errors.Wrap(err, "backup: closing tar")
	}
	if err := gz.Close(); err != nil {
		return Manifest{}, errors.Wrap(err, "backup: closing gzip")
	}
	return manifest, nil
}

// sanitize strips secrets for a shareable diagnostics export: storage driver
// credentials (Addition holds the whole per-driver config blob) and user
// password/OTP material. It intentionally leaves everything else intact so the
// export is still structurally faithful.
func sanitize(arc *Archive) {
	for i := range arc.Storages {
		arc.Storages[i].Addition = "{}"
	}
	for i := range arc.Users {
		arc.Users[i].Password = ""
		arc.Users[i].PwdHash = ""
		arc.Users[i].Salt = ""
		arc.Users[i].OtpSecret = ""
		arc.Users[i].Authn = ""
	}
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0600,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return errors.Wrapf(err, "backup: tar header %s", name)
	}
	if _, err := tw.Write(data); err != nil {
		return errors.Wrapf(err, "backup: tar body %s", name)
	}
	return nil
}

// mustJSON marshals a slice that is always JSON-serializable (model structs and
// local records). A failure here is a programmer error, not a runtime input
// error, so it is surfaced via an empty-array fallback that later validation
// (row counts) would catch rather than panicking a request.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return b
}
