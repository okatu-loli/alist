package op

import (
	stderrors "errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// Setting
type SettingItemHook func(item *model.SettingItem) error

var settingItemHooks = map[string]SettingItemHook{
	conf.VideoTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.VideoTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.AudioTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.AudioTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.ImageTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.ImageTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.TextTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.TextTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.ProxyTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.ProxyTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.ProxyIgnoreHeaders: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.ProxyIgnoreHeaders] = strings.Split(item.Value, ",")
		return nil
	},
	conf.PrivacyRegs: func(item *model.SettingItem) error {
		regStrs := strings.Split(item.Value, "\n")
		regs := make([]*regexp.Regexp, 0, len(regStrs))
		for _, regStr := range regStrs {
			reg, err := regexp.Compile(regStr)
			if err != nil {
				return errors.WithStack(err)
			}
			regs = append(regs, reg)
		}
		conf.PrivacyReg = regs
		return nil
	},
	conf.FilenameCharMapping: func(item *model.SettingItem) error {
		err := utils.Json.UnmarshalFromString(item.Value, &conf.FilenameCharMap)
		if err != nil {
			return err
		}
		log.Debugf("filename char mapping: %+v", conf.FilenameCharMap)
		return nil
	},
	conf.IgnoreDirectLinkParams: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.IgnoreDirectLinkParams] = strings.Split(item.Value, ",")
		return nil
	},
	conf.DefaultRole: func(item *model.SettingItem) error {
		v := strings.TrimSpace(item.Value)
		if v == "" {
			return nil
		}
		id, err := strconv.Atoi(v)
		if err != nil {
			return errors.WithStack(err)
		}
		_, err = GetRole(uint(id))
		return err
	},
	// site_theme (theming spec §J4, see site_theme.go): validated on every
	// save so a bad/oversized/js-carrying value never reaches the DB (and
	// therefore never reaches the PUBLIC /api/public/settings response).
	conf.SiteTheme: func(item *model.SettingItem) error {
		return validateSiteTheme(item.Value)
	},
	// market_installed_themes (theming spec §J5, see market_themes.go):
	// validated on every save — bad JSON/oversized/duplicate-id values never
	// reach the DB. Unlike site_theme, js IS allowed (see the doc comment on
	// validateMarketThemes for why that's safe).
	conf.MarketThemes: func(item *model.SettingItem) error {
		return validateMarketThemes(item.Value)
	},
}

func RegisterSettingItemHook(key string, hook SettingItemHook) {
	settingItemHooks[key] = hook
}

func HandleSettingItemHook(item *model.SettingItem) (hasHook bool, err error) {
	if hook, ok := settingItemHooks[item.Key]; ok {
		if hookErr := hook(item); hookErr != nil {
			return true, newSettingValidationError(hookErr)
		}
		return true, nil
	}
	return false, nil
}

// SettingValidationError wraps an error returned by a per-key
// settingItemHook (validateSiteTheme, validateMarketThemes, the DefaultRole/
// FilenameCharMapping/PrivacyRegs hooks above, ...) — i.e. the SAVED VALUE
// itself failed validation, as opposed to a genuine internal/database
// failure. HTTP callers (server/handles/setting.go's SaveSettings) use
// IsSettingValidationError to respond 400 (client input error) instead of
// 500 for this class of failure — a hook's whole job is validating client-
// supplied input, so every error it returns is inherently that, never a
// server-side fault.
type SettingValidationError struct {
	err error
}

func (e *SettingValidationError) Error() string { return e.err.Error() }
func (e *SettingValidationError) Unwrap() error { return e.err }

func newSettingValidationError(err error) error {
	if err == nil {
		return nil
	}
	return &SettingValidationError{err: err}
}

// IsSettingValidationError reports whether err — possibly a merged aggregate
// from SaveSettingItems (utils.MergeErrors flattens multiple errors into one,
// but SaveSettingItems only re-wraps the merged result when EVERY underlying
// failure was itself a validation error; see its doc comment) — resulted
// solely from setting-value validation hooks rejecting input, as opposed to
// any genuine internal/db failure.
func IsSettingValidationError(err error) bool {
	var v *SettingValidationError
	return stderrors.As(err, &v)
}

// Storage
type StorageHook func(typ string, storage driver.Driver)

var storageHooks = make([]StorageHook, 0)

func callStorageHooks(typ string, storage driver.Driver) {
	for _, hook := range storageHooks {
		hook(typ, storage)
	}
}

func RegisterStorageHook(hook StorageHook) {
	storageHooks = append(storageHooks, hook)
}

// Obj relocation ------------------------------------------------------------

// RelocateKind classifies filesystem mutations that move or delete an object,
// so anchored data (meta, and later shares/permissions) can follow it.
type RelocateKind uint8

const (
	RelocateMove RelocateKind = iota
	RelocateRename
	RelocateRemove
)

// ObjRelocateHook observes a successful move/rename/remove inside a storage.
// Paths are storage-internal (relative to the mount point), cleaned, absolute.
// newRel is empty for RelocateRemove.
type ObjRelocateHook = func(storageID uint, kind RelocateKind, oldRel, newRel string, isDir bool)

var objRelocateHooks []ObjRelocateHook

func RegisterObjRelocateHook(hook ObjRelocateHook) {
	objRelocateHooks = append(objRelocateHooks, hook)
}

func fireObjRelocate(storage driver.Driver, kind RelocateKind, oldRel, newRel string, isDir bool) {
	// Normalize to the balance group's primary member: GetBalancedStorage
	// round-robins which member serves a given fs op, but meta anchors are
	// recorded against the primary (resolvePathAnchor / balanceGroupPrimary).
	// Using storage's own ID here would make a relocation performed on a
	// non-anchor member invisible to WHERE storage_id=? lookups, silently
	// detaching the anchored meta from the object it protects.
	sid := balanceGroupPrimary(storage).GetStorage().ID
	for i, h := range objRelocateHooks {
		callObjRelocateHook(i, h, sid, kind, oldRel, newRel, isDir)
	}
}

// callObjRelocateHook runs a single hook behind recover: a hook panicking
// (e.g. a buggy third-party listener) must not abort the fs mutation it is
// merely observing, nor prevent the remaining hooks from firing.
func callObjRelocateHook(index int, h ObjRelocateHook, sid uint, kind RelocateKind, oldRel, newRel string, isDir bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("obj relocate hook #%d panicked (storage %d, kind %d, %s -> %s): %v", index, sid, kind, oldRel, newRel, r)
		}
	}()
	h(sid, kind, oldRel, newRel, isDir)
}

// Objs update ---------------------------------------------------------------

// ObjsUpdateHook observes a FRESH directory listing produced by op.List. parent
// is the full virtual path listed; objs are its direct children exactly as the
// driver returned them — op.List performs no per-user filtering, so listeners
// (the search auto-update, wave A7) see the RAW, complete listing. Fired only on
// a fresh storage.List, never on a cache hit (the objs did not change then).
type ObjsUpdateHook = func(parent string, objs []model.Obj)

var objsUpdateHooks []ObjsUpdateHook

func RegisterObjsUpdateHook(hook ObjsUpdateHook) {
	objsUpdateHooks = append(objsUpdateHooks, hook)
}

// HandleObjsUpdateHook fires every registered objs-update hook with the fresh
// listing. Each hook runs behind recover so a panicking observer neither aborts
// the list it is watching nor blocks the remaining hooks.
func HandleObjsUpdateHook(parent string, objs []model.Obj) {
	for i, h := range objsUpdateHooks {
		callObjsUpdateHook(i, h, parent, objs)
	}
}

func callObjsUpdateHook(index int, h ObjsUpdateHook, parent string, objs []model.Obj) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("objs update hook #%d panicked (parent %s): %v", index, parent, r)
		}
	}()
	h(parent, objs)
}
