package op

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/generic_sync"
	"github.com/alist-org/alist/v3/pkg/utils"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// Although the driver type is stored,
// there is a storage in each driver,
// so it should actually be a storage, just wrapped by the driver
var storagesMap generic_sync.MapOf[string, driver.Driver]

func GetAllStorages() []driver.Driver {
	return storagesMap.Values()
}

func HasStorage(mountPath string) bool {
	return storagesMap.Has(utils.FixAndCleanPath(mountPath))
}

func GetStorageByMountPath(mountPath string) (driver.Driver, error) {
	mountPath = utils.FixAndCleanPath(mountPath)
	storageDriver, ok := storagesMap.Load(mountPath)
	if !ok {
		return nil, errors.Errorf("no mount path for an storage is: %s", mountPath)
	}
	return storageDriver, nil
}

func firstPathSegment(p string) string {
	p = utils.FixAndCleanPath(p)
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i]
	}
	return p
}

// CreateStorage Save the storage to database so storage can get an id
// then instantiate corresponding driver and save it in memory
func CreateStorage(ctx context.Context, storage model.Storage) (uint, error) {
	storage.Modified = time.Now()
	storage.MountPath = utils.FixAndCleanPath(storage.MountPath)

	//if storage.MountPath == "/" {
	//	return 0, errors.New("Mount path cannot be '/'")
	//}

	var err error
	// check driver first
	driverName := storage.Driver
	driverNew, err := GetDriver(driverName)
	if err != nil {
		return 0, errors.WithMessage(err, "failed get driver new")
	}
	storageDriver := driverNew()
	// [Minor 10] storage.Addition here is entirely fresh, caller-submitted
	// input (a create has no "old value" to merge in) — reject it up front
	// if a secret field literally collides with the reserved enc:v1: prefix,
	// before encryptStorageAddition's "already encrypted, skip" check could
	// otherwise mistake it for real ciphertext and store it unencrypted.
	if err := rejectReservedPrefixAddition(driverName, storage.Addition); err != nil {
		return 0, errors.WithMessage(err, "invalid storage addition")
	}
	// G4 §2: encrypt Secret-marked Additional fields before they ever touch
	// the database — encryptStorageAddition is idempotent (skips a field
	// already carrying the enc:v1: prefix) and a no-op for a driver with no
	// secret fields.
	storage.Addition, err = encryptStorageAddition(driverName, storage.Addition)
	if err != nil {
		return 0, errors.WithMessage(err, "failed to encrypt storage secrets")
	}
	// insert storage to database
	err = db.CreateStorage(&storage)
	if err != nil {
		return storage.ID, errors.WithMessage(err, "failed create storage in database")
	}
	// already has an id
	err = initStorage(ctx, storage, storageDriver)
	go callStorageHooks("add", storageDriver)
	if err != nil {
		return storage.ID, errors.Wrap(err, "failed init storage but storage is already created")
	}
	log.Debugf("storage %+v is created", storageDriver)
	return storage.ID, nil
}

// LoadStorage load exist storage in db to memory
func LoadStorage(ctx context.Context, storage model.Storage) error {
	storage.MountPath = utils.FixAndCleanPath(storage.MountPath)
	// check driver first
	driverName := storage.Driver
	driverNew, err := GetDriver(driverName)
	if err != nil {
		return errors.WithMessage(err, "failed get driver new")
	}
	storageDriver := driverNew()

	err = initStorage(ctx, storage, storageDriver)
	go callStorageHooks("add", storageDriver)
	// First load is the v3→v4 migration's anchor-backfill channel: adopt any
	// permission/meta rows whose Path now falls under this freshly-loaded
	// storage into the node registry (idempotent, best-effort — rows under
	// storages not yet loaded are picked up when those load).
	adoptNodelessAnchors()
	log.Debugf("storage %+v is created", storageDriver)
	return err
}

func getCurrentGoroutineStack() string {
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

// initStorage initialize the driver and store to storagesMap
func initStorage(ctx context.Context, storage model.Storage, storageDriver driver.Driver) (err error) {
	storageDriver.SetStorage(storage)
	driverStorage := storageDriver.GetStorage()

	// additionLoaded tracks whether storageDriver.GetAddition() (the driver's
	// TYPED Addition struct — e.g. its Password field) was ever successfully
	// populated from the (decrypted) stored Addition string below.
	//
	// [CRITICAL, G4 red team] This exists because of a real destructive bug:
	// persistDriverStorage below (both here and in MustSaveDriverStorage's
	// other callers) derives the persisted Addition column from
	// driver.GetAddition() — that's how a refreshed OAuth token, or a
	// lazily-migrated legacy plaintext row, gets written back. If decryption
	// or JSON-unmarshaling never populated that typed struct (wrong/missing
	// secrets key, corrupt ciphertext), it is still its ZERO VALUE — and
	// unconditionally re-deriving Addition from it would marshal that zero
	// value, "encrypt" it (sealing empty strings), and overwrite the row's
	// real ciphertext with it. That turns a RECOVERABLE operational mistake
	// (secrets key lost, or a backup restored without also restoring the
	// key — see internal/backup's IncludeSecretsKey) into an IRREVERSIBLE
	// one: LoadStorages calls this at boot for every row, so one wrong key
	// would have silently zeroed out every stored credential in the
	// database, forever, the very first time the server started with it.
	//
	// driverStorage.Addition itself (the raw string, as opposed to the typed
	// struct) is never touched by anything in this function — SetStorage
	// above copied the DB row's original value into it and it stays exactly
	// that until persistDriverStorage's own encrypt/marshal step runs — so
	// when additionLoaded is false, persisting driverStorage AS IS (status
	// change only, Addition untouched) is always a safe, no-data-loss write.
	additionLoaded := false
	persistDriverStorage := func() {
		if additionLoaded {
			MustSaveDriverStorage(storageDriver)
			return
		}
		if err := db.UpdateStorage(driverStorage); err != nil {
			log.Errorf("failed save driver storage status: %s", err)
		}
	}

	defer func() {
		if r := recover(); r != nil {
			errInfo := fmt.Sprintf("[panic] err: %v\nstack: %s\n", r, getCurrentGoroutineStack())
			log.Errorf("panic init storage: %s", errInfo)
			driverStorage.SetStatus(errInfo)
			persistDriverStorage()
			storagesMap.Store(driverStorage.MountPath, storageDriver)
		}
	}()
	// Unmarshal Addition. G4 §2: decrypt any Secret-marked field first (a
	// value with no enc:v1: prefix is passed through as-is — a pre-G4
	// plaintext row, or a driver with no secret fields — so this never
	// disturbs a storage that predates encryption). A decrypt failure (wrong/
	// missing secrets key, corrupt ciphertext) fails loud through the exact
	// same path a JSON-unmarshal failure already takes below: driverStorage's
	// Status is set to the error, persistDriverStorage still persists that
	// status (WITHOUT touching Addition — see additionLoaded above), and
	// initStorage returns the wrapped error — never a silently garbage/empty
	// credential handed to the driver, and never a destroyed ciphertext.
	addition, err := decryptStorageAddition(driverStorage.Driver, driverStorage.Addition)
	if err == nil {
		err = utils.Json.UnmarshalFromString(addition, storageDriver.GetAddition())
	}
	additionLoaded = err == nil
	if err == nil {
		if ref, ok := storageDriver.(driver.Reference); ok {
			if strings.HasPrefix(driverStorage.Remark, "ref:/") {
				refMountPath := driverStorage.Remark
				i := strings.Index(refMountPath, "\n")
				if i > 0 {
					refMountPath = refMountPath[4:i]
				} else {
					refMountPath = refMountPath[4:]
				}
				var refStorage driver.Driver
				refStorage, err = GetStorageByMountPath(refMountPath)
				if err != nil {
					err = fmt.Errorf("ref: %w", err)
				} else {
					err = ref.InitReference(refStorage)
					if err != nil && errs.IsNotSupportError(err) {
						err = fmt.Errorf("ref: storage is not %s", storageDriver.Config().Name)
					}
				}
			}
		}
	}
	if err == nil {
		err = storageDriver.Init(ctx)
	}
	storagesMap.Store(driverStorage.MountPath, storageDriver)
	if err != nil {
		driverStorage.SetStatus(err.Error())
		err = errors.Wrap(err, "failed init storage")
	} else {
		driverStorage.SetStatus(WORK)
	}
	persistDriverStorage()
	return err
}

func EnableStorage(ctx context.Context, id uint) error {
	storage, err := db.GetStorageById(id)
	if err != nil {
		return errors.WithMessage(err, "failed get storage")
	}
	if !storage.Disabled {
		return errors.Errorf("this storage have enabled")
	}
	storage.Disabled = false
	err = db.UpdateStorage(storage)
	if err != nil {
		return errors.WithMessage(err, "failed update storage in db")
	}
	err = LoadStorage(ctx, *storage)
	if err != nil {
		return errors.WithMessage(err, "failed load storage")
	}
	return nil
}

func DisableStorage(ctx context.Context, id uint) error {
	storage, err := db.GetStorageById(id)
	if err != nil {
		return errors.WithMessage(err, "failed get storage")
	}
	if storage.Disabled {
		return errors.Errorf("this storage have disabled")
	}
	storageDriver, err := GetStorageByMountPath(storage.MountPath)
	if err != nil {
		return errors.WithMessage(err, "failed get storage driver")
	}
	// drop the storage in the driver
	if err := storageDriver.Drop(ctx); err != nil {
		return errors.Wrap(err, "failed drop storage")
	}
	// delete the storage in the memory
	storage.Disabled = true
	storage.SetStatus(DISABLED)
	err = db.UpdateStorage(storage)
	if err != nil {
		return errors.WithMessage(err, "failed update storage in db")
	}
	storagesMap.Delete(storage.MountPath)
	go callStorageHooks("del", storageDriver)
	return nil
}

// UpdateStorage update storage
// get old storage first
// drop the storage then reinitialize
func UpdateStorage(ctx context.Context, storage model.Storage) error {
	oldStorage, err := db.GetStorageById(storage.ID)
	if err != nil {
		return errors.WithMessage(err, "failed get old storage")
	}
	if oldStorage.Driver != storage.Driver {
		return errors.Errorf("driver cannot be changed")
	}
	// [Minor 10] Check the CALLER-SUBMITTED value here, before the merge
	// below folds the old (possibly genuinely-enc:v1:-prefixed) stored value
	// in for a blank field — merging first would make a legitimate
	// already-ciphertext value indistinguishable from the case this rejects
	// (see crypto.RejectReservedPrefix's doc comment). A field the admin left
	// blank (meaning "keep the old value") never reaches this check as a
	// literal enc:v1:-prefixed value FROM THEM — MergeStorageAdditionSecrets
	// only substitutes it in afterwards.
	if err := rejectReservedPrefixAddition(storage.Driver, storage.Addition); err != nil {
		return errors.WithMessage(err, "invalid storage addition")
	}
	// G3 §1: a blank secret Additional field (password, API token, ...) in
	// the incoming update means "leave it as-is" — the admin UI never
	// received the real value back (RedactStorageAddition), so it can only
	// ever resubmit blank for a field it didn't deliberately change. Mirrors
	// UpdateSSOProvider's "blank client_secret keeps the stored one" rule.
	storage.Addition = MergeStorageAdditionSecrets(storage.Driver, oldStorage.Addition, storage.Addition)
	// G4 §2: encrypt whatever the merge above landed on (a freshly-submitted
	// plaintext value, or the old row's value carried through verbatim —
	// which may already be encrypted; encryptStorageAddition's enc:v1:
	// prefix check makes re-encrypting it a no-op).
	storage.Addition, err = encryptStorageAddition(storage.Driver, storage.Addition)
	if err != nil {
		return errors.WithMessage(err, "failed to encrypt storage secrets")
	}
	storage.Modified = time.Now()
	storage.MountPath = utils.FixAndCleanPath(storage.MountPath)
	//if storage.MountPath == "/" {
	//	return errors.New("Mount path cannot be '/'")
	//}

	// A storage that ends up enabled is always Drop()ped and reinitialized
	// below against oldStorage.MountPath's live driver instance — unlike
	// CreateStorage/LoadStorage there is no from-scratch path here. If that
	// driver isn't currently loaded (e.g. the storage is disabled today and
	// DisableStorage already removed it from storagesMap), there is nothing
	// to Drop/reinit and the call can never succeed. Resolve that
	// precondition up front, before db.UpdateStorage below persists the new
	// row: previously the persist happened unconditionally first, so a
	// doomed "enable" (optionally combined with a rename) update still left
	// the storage row permanently rewritten in the database with no driver
	// ever backing it — a retry sees the already-updated oldStorage and
	// fails the same way forever (see storage_update_driver_check_test.go).
	var storageDriver driver.Driver
	if !storage.Disabled {
		storageDriver, err = GetStorageByMountPath(oldStorage.MountPath)
		if err != nil {
			return errors.WithMessage(err, "failed get storage driver")
		}
	}

	err = db.UpdateStorage(&storage)
	if err != nil {
		return errors.WithMessage(err, "failed update storage in database")
	}
	if storageDriver != nil {
		ClearCache(storageDriver, "/")
	}
	if storage.Disabled {
		return nil
	}
	mountPathRenamed := oldStorage.MountPath != storage.MountPath
	var modifiedRoleIDs []uint
	if mountPathRenamed {
		// storageDriver's presence was already confirmed above, before the
		// persist, so no further error check is needed here.
		// mount path renamed, need to drop the storage
		storagesMap.Delete(oldStorage.MountPath)
		var updErr error
		modifiedRoleIDs, updErr = db.UpdateRolePermissionsPathPrefix(oldStorage.MountPath, storage.MountPath)
		if updErr != nil {
			return errors.WithMessage(updErr, "failed to update role permissions")
		}
	}
	err = storageDriver.Drop(ctx)
	if err != nil {
		return errors.Wrapf(err, "failed drop storage")
	}

	err = initStorage(ctx, storage, storageDriver)
	if mountPathRenamed {
		// UpdateRolePermissionsPathPrefix only rewrote the role row's
		// PermissionScopes JSON column; the normalized scope-permission rows
		// the policy engine actually reads (op.ResolveEffectivePolicy) still
		// need to be re-derived. scopePermissionRows resolves each entry's
		// StorageID/Anchor via ResolvePathAnchor, which looks up the mount
		// path in storagesMap. The new mount path only becomes resolvable
		// once initStorage above has stored it (initStorage stores it
		// unconditionally, even if driver Init itself failed), so this loop
		// must run after initStorage, not before: running it earlier — while
		// the old mount was already deleted and the new one not yet
		// registered — made ResolvePathAnchor fail and silently left every
		// resynced row's StorageID/Anchor zeroed (see
		// storage_rename_scope_sync_test.go).
		for _, id := range modifiedRoleIDs {
			roleCache.Del(fmt.Sprint(id))

			role, roleErr := db.GetRole(id)
			if roleErr != nil {
				return errors.WithMessage(roleErr, "failed to reload role after path rename")
			}
			if roleErr := db.UpdateRoleWithScopePermissions(role, scopePermissionRows(role)); roleErr != nil {
				return errors.WithMessage(roleErr, "failed to resync role scope permissions")
			}
			invalidateRolePolicy(id)

			users, usersErr := db.GetUsersByRole(int(id))
			if usersErr != nil {
				return errors.WithMessage(usersErr, "failed to get users by role")
			}
			for _, user := range users {
				userCache.Del(user.Username)
			}
		}

		// The scope resync above only covers scope-origin rows (rebuilt from
		// each role's declaration). Manual permission and meta rows are
		// node-anchored: their derived path joins the storage's LIVE mount point
		// (NodeVirtualPath looks it up fresh each time), so a mount rename needs
		// no per-row path rewrite — only the path-derived caches must be dropped
		// so they re-derive against the new mount. The scope resync above stays:
		// scope rows keep Path as their truth.
		invalidateAllPolicy()
		invalidateMetaIndex()
	}
	go callStorageHooks("update", storageDriver)
	log.Debugf("storage %+v is update", storageDriver)
	return err
}

func DeleteStorageById(ctx context.Context, id uint) error {
	storage, err := db.GetStorageById(id)
	if err != nil {
		return errors.WithMessage(err, "failed get storage")
	}
	firstMount := firstPathSegment(storage.MountPath)
	if firstMount != "" {
		roles, err := db.GetAllRoles()
		if err != nil {
			return errors.WithMessage(err, "failed to load roles")
		}
		var usedBy []string
		for _, r := range roles {
			for _, entry := range r.PermissionScopes {
				if firstPathSegment(entry.Path) == firstMount {
					usedBy = append(usedBy, "role:"+r.Name)
					break
				}
			}
		}
		if len(usedBy) > 0 {
			return errors.Errorf("storage is used by %s, please cancel usage first", strings.Join(usedBy, ", "))
		}
	}
	if !storage.Disabled {
		storageDriver, err := GetStorageByMountPath(storage.MountPath)
		if err != nil {
			return errors.WithMessage(err, "failed get storage driver")
		}
		// drop the storage in the driver
		if err := storageDriver.Drop(ctx); err != nil {
			return errors.Wrapf(err, "failed drop storage")
		}
		// delete the storage in the memory
		storagesMap.Delete(storage.MountPath)
		go callStorageHooks("del", storageDriver)
	}
	// delete the storage in the database
	if err := db.DeleteStorageById(id); err != nil {
		return errors.WithMessage(err, "failed delete storage in database")
	}
	// Node-anchored permission/meta rows on this storage now derive no path
	// (its nodes reference a gone storage); drop the path-derived caches so
	// those rows stop participating in decisions (spec §7). The rows and their
	// nodes are intentionally left in the DB for admin cleanup / re-mount.
	invalidateAllPolicy()
	invalidateMetaIndex()
	return nil
}

// MustSaveDriverStorage call from specific driver
func MustSaveDriverStorage(driver driver.Driver) {
	err := saveDriverStorage(driver)
	if err != nil {
		log.Errorf("failed save driver storage: %s", err)
	}
}

func saveDriverStorage(driver driver.Driver) error {
	storage := driver.GetStorage()
	addition := driver.GetAddition()
	str, err := utils.Json.MarshalToString(addition)
	if err != nil {
		return errors.Wrap(err, "error while marshal addition")
	}
	// G4 §2: this is the driver's own self-save path (e.g. persisting a
	// refreshed OAuth token back onto its Addition) — it never goes through
	// CreateStorage/UpdateStorage's encrypt calls, so it needs its own.
	str, err = encryptStorageAddition(storage.Driver, str)
	if err != nil {
		return errors.WithMessage(err, "failed to encrypt storage secrets")
	}
	storage.Addition = str
	err = db.UpdateStorage(storage)
	if err != nil {
		return errors.WithMessage(err, "failed update storage in database")
	}
	return nil
}

// getStoragesByPath get storage by longest match path, contains balance storage.
// for example, there is /a/b,/a/c,/a/d/e,/a/d/e.balance
// getStoragesByPath(/a/d/e/f) => /a/d/e,/a/d/e.balance
func getStoragesByPath(path string) []driver.Driver {
	storages := make([]driver.Driver, 0)
	curSlashCount := 0
	storagesMap.Range(func(mountPath string, value driver.Driver) bool {
		mountPath = utils.GetActualMountPath(mountPath)
		// is this path
		if utils.IsSubPath(mountPath, path) {
			slashCount := strings.Count(utils.PathAddSeparatorSuffix(mountPath), "/")
			// not the longest match
			if slashCount > curSlashCount {
				storages = storages[:0]
				curSlashCount = slashCount
			}
			if slashCount == curSlashCount {
				storages = append(storages, value)
			}
		}
		return true
	})
	// make sure the order is the same for same input
	sort.Slice(storages, func(i, j int) bool {
		return storages[i].GetStorage().MountPath < storages[j].GetStorage().MountPath
	})
	return storages
}

// GetStorageVirtualFilesByPath Obtain the virtual file generated by the storage according to the path
// for example, there are: /a/b,/a/c,/a/d/e,/a/b.balance1,/av
// GetStorageVirtualFilesByPath(/a) => b,c,d
func GetStorageVirtualFilesByPath(prefix string) []model.Obj {
	files := make([]model.Obj, 0)
	storages := storagesMap.Values()
	sort.Slice(storages, func(i, j int) bool {
		if storages[i].GetStorage().Order == storages[j].GetStorage().Order {
			return storages[i].GetStorage().MountPath < storages[j].GetStorage().MountPath
		}
		return storages[i].GetStorage().Order < storages[j].GetStorage().Order
	})

	prefix = utils.FixAndCleanPath(prefix)
	set := mapset.NewSet[string]()
	for _, v := range storages {
		mountPath := utils.GetActualMountPath(v.GetStorage().MountPath)
		// Exclude prefix itself and non prefix
		if len(prefix) >= len(mountPath) || !utils.IsSubPath(prefix, mountPath) {
			continue
		}
		name := strings.SplitN(strings.TrimPrefix(mountPath[len(prefix):], "/"), "/", 2)[0]
		if set.Add(name) {
			files = append(files, &model.Object{
				Name:     name,
				Size:     0,
				Modified: v.GetStorage().Modified,
				IsFolder: true,
			})
		}
	}
	return files
}

var balanceMap generic_sync.MapOf[string, int]

// GetBalancedStorage get storage by path
func GetBalancedStorage(path string) driver.Driver {
	path = utils.FixAndCleanPath(path)
	storages := getStoragesByPath(path)
	storageNum := len(storages)
	switch storageNum {
	case 0:
		return nil
	case 1:
		return storages[0]
	default:
		virtualPath := utils.GetActualMountPath(storages[0].GetStorage().MountPath)
		i, _ := balanceMap.LoadOrStore(virtualPath, 0)
		i = (i + 1) % storageNum
		balanceMap.Store(virtualPath, i)
		return storages[i]
	}
}

// balanceGroupPrimary returns the deterministic representative storage for
// storage's balance group. Anchored data (meta, and later shares/permissions)
// must record ONE storage_id for a whole group regardless of which member an
// fs op actually ran on (GetBalancedStorage round-robins between members on
// every call) — otherwise a lookup keyed by the member that happened to serve
// a later op misses data anchored via an earlier op that hit a different
// member.
//
// The representative is: the bare member whose MountPath equals the group's
// virtual mount path (utils.GetActualMountPath(MountPath) strips no suffix
// off it, i.e. it carries no ".balanceN" suffix at all). If the group has no
// bare member (every member is suffixed), the member whose MountPath sorts
// lexicographically smallest stands in instead — still a fixed, deterministic
// choice, just not "the" canonical one.
//
// A non-balance storage is trivially its own primary.
func balanceGroupPrimary(storage driver.Driver) driver.Driver {
	virtualPath := utils.GetActualMountPath(storage.GetStorage().MountPath)
	primary := storage
	found := false
	storagesMap.Range(func(mountPath string, value driver.Driver) bool {
		if utils.GetActualMountPath(mountPath) != virtualPath {
			return true
		}
		if mountPath == virtualPath {
			primary = value
			found = true
			return false // bare member found: it wins outright, stop scanning
		}
		if !found && mountPath < primary.GetStorage().MountPath {
			primary = value
		}
		return true
	})
	return primary
}
