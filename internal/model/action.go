package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// Action is the RBAC5 verb vocabulary that replaces the legacy 17-bit
// permission mask. ActRead has no legacy bit: in the old model "having any
// scope covering the path" implied read, so the 0002 migration adds ActRead
// explicitly to every converted permission.
type Action string

// Resource types for Permission.ResourceType. A permission row is matched
// only within its own resource type (see op.ResolveEffectivePolicy): fs rows
// gate filesystem paths, system rows gate admin control-plane domains.
const (
	ResourceFS     = "fs"
	ResourceSystem = "system"
)

const (
	ActRead             Action = "read"
	ActSeeHides         Action = "see_hides"
	ActAccessWithoutPwd Action = "access_without_password"
	ActOfflineDownload  Action = "offline_download"
	ActWrite            Action = "write"
	ActRename           Action = "rename"
	ActMove             Action = "move"
	ActCopy             Action = "copy"
	ActRemove           Action = "remove"
	ActWebdavRead       Action = "webdav_read"
	ActWebdavManage     Action = "webdav_manage"
	ActFTPAccess        Action = "ftp_access"
	ActFTPManage        Action = "ftp_manage"
	ActReadArchives     Action = "read_archives"
	ActDecompress       Action = "decompress"
	ActPathLimit        Action = "path_limit"
	ActMCPAccess        Action = "mcp_access"
	ActMCPManage        Action = "mcp_manage"
)

// LegacyBitActions maps legacy permission bit index -> action.
// Order MUST match the bit comments on model.User.Permission and the
// Perm* constants in server/common/role_perm.go.
var LegacyBitActions = [17]Action{
	ActSeeHides, ActAccessWithoutPwd, ActOfflineDownload, ActWrite,
	ActRename, ActMove, ActCopy, ActRemove,
	ActWebdavRead, ActWebdavManage, ActFTPAccess, ActFTPManage,
	ActReadArchives, ActDecompress, ActPathLimit, ActMCPAccess, ActMCPManage,
}

// SystemAction builds the fully-qualified name of a system (admin
// control-plane) action: "system.<domain>.<verb>" (spec §2.3). Identity lives
// in the action string itself, so ResourceType stays an index/hint rather than
// the sole correctness dependency. system actions are NOT part of the
// LegacyBitActions bridge — they carry no legacy mask bit.
func SystemAction(domain, verb string) Action {
	return Action("system." + domain + "." + verb)
}

func ActionsFromLegacyBits(perm int32) Actions {
	var out Actions
	for bit, act := range LegacyBitActions {
		if (perm>>bit)&1 == 1 {
			out = append(out, act)
		}
	}
	return out
}

type Actions []Action

func (as Actions) Contains(a Action) bool {
	for _, v := range as {
		if v == a {
			return true
		}
	}
	return false
}

func (as Actions) LegacyBits() int32 {
	var bits int32
	for bit, act := range LegacyBitActions {
		if as.Contains(act) {
			bits |= 1 << bit
		}
	}
	return bits
}

func (as Actions) Value() (driver.Value, error) {
	if len(as) == 0 {
		return "[]", nil
	}
	bs, err := json.Marshal(as)
	return string(bs), err
}

func (as *Actions) Scan(value interface{}) error {
	switch v := value.(type) {
	case []byte:
		return json.Unmarshal(v, as)
	case string:
		return json.Unmarshal([]byte(v), as)
	case nil:
		*as = nil
		return nil
	default:
		return fmt.Errorf("cannot scan %T into Actions", value)
	}
}
