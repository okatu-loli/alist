package handles

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// Red-team regressions for G3 §1 (secret as a first-class citizen, step 1:
// declare + read-back sentinel). Mirrors sso_redteam_test.go's shape one
// storage layer down: a stored credential (here, a driver.Item marked
// Secret) must never round-trip through the admin read APIs, and a routine
// save that never received the real value back must not wipe it.

// secretRedteamAddition is a minimal Additional struct with the SAME
// confidential:"true" tag drivers/crypt/meta.go's Password/Salt carry, so
// this test exercises the real getAdditionalItems tag-reading path (G3 §1's
// "activate the dead tag") without depending on Crypt's own runtime
// requirements (a valid backing RemotePath, actual encrypt/decrypt).
type secretRedteamAddition struct {
	RemotePath string `json:"remote_path"`
	Password   string `json:"password" confidential:"true"`
	Salt       string `json:"salt" confidential:"true"`
}

type secretRedteamDriver struct {
	model.Storage
	Addition secretRedteamAddition
}

func (d *secretRedteamDriver) Config() driver.Config {
	return driver.Config{Name: "SecretRedteamDriver", NoCache: true}
}
func (d *secretRedteamDriver) GetAddition() driver.Additional { return &d.Addition }
func (d *secretRedteamDriver) Init(ctx context.Context) error { return nil }
func (d *secretRedteamDriver) Drop(ctx context.Context) error { return nil }
func (d *secretRedteamDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}
func (d *secretRedteamDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return nil, nil
}

func init() {
	op.RegisterDriver(func() driver.Driver { return &secretRedteamDriver{} })
}

const secretRedteamValue = "s3cr3t-must-not-leak"

// liveSecretRedteamDriver fetches the live, currently-mounted driver
// instance for mountPath and asserts it as *secretRedteamDriver. Its
// Addition field holds whatever initStorage last decrypted (G4 §2) — this
// is the "does the driver itself still have a working, correct credential"
// check the DB-encrypted-at-rest assertions below use instead of reading
// db.GetStorageById's raw (now enc:v1:-prefixed) Addition string directly.
func liveSecretRedteamDriver(t *testing.T, mountPath string) *secretRedteamDriver {
	t.Helper()
	d, err := op.GetStorageByMountPath(mountPath)
	if err != nil {
		t.Fatalf("get live driver for %s: %v", mountPath, err)
	}
	sd, ok := d.(*secretRedteamDriver)
	if !ok {
		t.Fatalf("live driver for %s is %T, want *secretRedteamDriver", mountPath, d)
	}
	return sd
}

// createSecretRedteamStorage creates a live SecretRedteamDriver storage with
// a distinguishing secret password, cleaning it up on test end.
func createSecretRedteamStorage(t *testing.T, mountPath, password string) uint {
	t.Helper()
	addition, err := json.Marshal(secretRedteamAddition{RemotePath: "/target", Password: password, Salt: "pepper"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mountPath,
		Driver:    "SecretRedteamDriver",
		Addition:  string(addition),
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	return id
}

// TestRedteam_StorageListNeverEchoesSecretValue pins ListStorages: the
// secret password must never appear in the response body, and the
// redacted-field's "_set" sentinel must report true.
func TestRedteam_StorageListNeverEchoesSecretValue(t *testing.T) {
	createSecretRedteamStorage(t, "/secret-redteam-list", secretRedteamValue)

	w := getJSON(handlerAdmin(), ListStorages, "/api/admin/storage/list?per_page=0")
	if code := respCode(t, w); code != 200 {
		t.Fatalf("ListStorages: want 200, got %d: %s", code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, secretRedteamValue) {
		t.Fatalf("REGRESSION: secret password leaked into storage list: %s", body)
	}
	if !strings.Contains(body, `\"password_set\":true`) {
		t.Fatalf("storage list must report password_set:true for a configured secret: %s", body)
	}
}

// TestRedteam_StorageGetNeverEchoesSecretValue pins GetStorage (single-item
// read) the same way.
func TestRedteam_StorageGetNeverEchoesSecretValue(t *testing.T) {
	id := createSecretRedteamStorage(t, "/secret-redteam-get", secretRedteamValue)

	w := getJSON(handlerAdmin(), GetStorage, "/api/admin/storage/get?id="+strconv.Itoa(int(id)))
	if code := respCode(t, w); code != 200 {
		t.Fatalf("GetStorage: want 200, got %d: %s", code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, secretRedteamValue) {
		t.Fatalf("REGRESSION: secret password leaked into storage get: %s", body)
	}
	if !strings.Contains(body, `\"password_set\":true`) {
		t.Fatalf("storage get must report password_set:true for a configured secret: %s", body)
	}

	// Sanity: the value really is stored and usable, so the absence above is
	// redaction (not a missing/broken write) — but G4 §2 means the DB row
	// itself must NOT hold the plaintext either: it must carry the enc:v1:
	// prefix (encrypted at rest), and only the LIVE driver — which
	// initStorage decrypted into on load — should ever see the real value.
	raw, err := db.GetStorageById(id)
	if err != nil {
		t.Fatalf("db read: %v", err)
	}
	if strings.Contains(raw.Addition, secretRedteamValue) {
		t.Fatalf("REGRESSION (G4): secret stored in plaintext in the DB row, want enc:v1:-encrypted: %s", raw.Addition)
	}
	var rawM map[string]any
	if err := json.Unmarshal([]byte(raw.Addition), &rawM); err != nil {
		t.Fatalf("decode raw DB addition: %v", err)
	}
	rawPassword, _ := rawM["password"].(string)
	if !strings.HasPrefix(rawPassword, "enc:v1:") {
		t.Fatalf("DB-stored password must carry the enc:v1: prefix, got %q", rawPassword)
	}
	if got := liveSecretRedteamDriver(t, "/secret-redteam-get").Addition.Password; got != secretRedteamValue {
		t.Fatalf("live driver's decrypted password = %q, want %q", got, secretRedteamValue)
	}
}

// TestRedteam_StorageUpdateWithoutSecretDoesNotWipeIt pins the "blank means
// keep" merge rule: an admin edit that changes an unrelated field
// (mount_path stays the same; here we flip remote_path) while resubmitting
// the redacted blank for password must NOT wipe the stored secret.
func TestRedteam_StorageUpdateWithoutSecretDoesNotWipeIt(t *testing.T) {
	id := createSecretRedteamStorage(t, "/secret-redteam-update", secretRedteamValue)

	// Simulate exactly what the admin UI would resubmit after a GET: the
	// redacted (blank) password, plus a changed non-secret field.
	body := `{"id":` + strconv.Itoa(int(id)) + `,"mount_path":"/secret-redteam-update","driver":"SecretRedteamDriver","addition":"{\"remote_path\":\"/changed\",\"password\":\"\",\"salt\":\"pepper\"}"}`
	w := postJSON(handlerAdmin(), UpdateStorage, "/api/admin/storage/update", body)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("UpdateStorage: want 200, got %d: %s", code, w.Body.String())
	}

	raw, err := db.GetStorageById(id)
	if err != nil {
		t.Fatalf("db read after update: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw.Addition), &m); err != nil {
		t.Fatalf("decode persisted addition: %v", err)
	}
	// The DB row itself must still be encrypted (G4 §2), not the plaintext
	// pinned in place — the merge (G3) and the encrypt step (G4) apply in
	// that order regardless of which one the old value came from.
	rawPassword, _ := m["password"].(string)
	if !strings.HasPrefix(rawPassword, "enc:v1:") {
		t.Fatalf("DB-stored password after update must still carry the enc:v1: prefix, got %q", rawPassword)
	}
	if m["remote_path"] != "/changed" {
		t.Fatalf("the deliberately-changed non-secret field must still take effect, got %v", m["remote_path"])
	}
	if got := liveSecretRedteamDriver(t, "/secret-redteam-update").Addition.Password; got != secretRedteamValue {
		t.Fatalf("REGRESSION: update with a blank password wiped the stored secret — live driver's decrypted password = %q, want %q", got, secretRedteamValue)
	}
}

// TestRedteam_StorageUpdateWithNewSecretOverwritesIt confirms the merge rule
// only protects BLANK resubmissions — an admin deliberately submitting a new
// non-blank secret must have it take effect, not be silently pinned to the
// old value forever.
func TestRedteam_StorageUpdateWithNewSecretOverwritesIt(t *testing.T) {
	id := createSecretRedteamStorage(t, "/secret-redteam-update-change", secretRedteamValue)

	const newSecret = "brand-new-secret-value"
	body := `{"id":` + strconv.Itoa(int(id)) + `,"mount_path":"/secret-redteam-update-change","driver":"SecretRedteamDriver","addition":"{\"remote_path\":\"/target\",\"password\":\"` + newSecret + `\",\"salt\":\"pepper\"}"}`
	w := postJSON(handlerAdmin(), UpdateStorage, "/api/admin/storage/update", body)
	if code := respCode(t, w); code != 200 {
		t.Fatalf("UpdateStorage: want 200, got %d: %s", code, w.Body.String())
	}

	raw, err := db.GetStorageById(id)
	if err != nil {
		t.Fatalf("db read after update: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw.Addition), &m); err != nil {
		t.Fatalf("decode persisted addition: %v", err)
	}
	rawPassword, _ := m["password"].(string)
	if !strings.HasPrefix(rawPassword, "enc:v1:") {
		t.Fatalf("DB-stored password after update must carry the enc:v1: prefix, got %q", rawPassword)
	}
	if got := liveSecretRedteamDriver(t, "/secret-redteam-update-change").Addition.Password; got != newSecret {
		t.Fatalf("an explicitly-submitted new secret must be honored — live driver's decrypted password = %q, want %q", got, newSecret)
	}
}
