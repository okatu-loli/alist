package labels

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:labels_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
}

func mountStorage(t *testing.T, mount string) uint {
	t.Helper()
	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + t.TempDir() + `"}`,
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })
	return sid
}

// Decorate must return the applied labels keyed by ref path, and nothing for
// refs with no node/binding — over only the refs it is handed.
func TestDecorateReturnsAppliedLabels(t *testing.T) {
	ctx := context.Background()
	mountStorage(t, "/dec")

	l := &model.Label{Name: "dec-review", Color: "#00ff00"}
	if err := op.CreateLabel(l); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteLabel(l.ID) })
	if err := op.ApplyLabel(ctx, "/dec/a", l.ID); err != nil {
		t.Fatalf("apply: %v", err)
	}

	refs := []ext.ObjRef{{Anchor: "/dec/a"}, {Anchor: "/dec/b"}}
	got, err := instance.Decorate(ctx, Group, refs)
	if err != nil {
		t.Fatalf("decorate: %v", err)
	}
	// /dec/a carries the label; /dec/b (no binding) is absent.
	bs, ok := got["/dec/a"].([]badge)
	if !ok || len(bs) != 1 || bs[0].ID != l.ID || bs[0].Name != "dec-review" || bs[0].Color != "#00ff00" {
		t.Fatalf("wrong badges for /dec/a: %#v", got["/dec/a"])
	}
	if _, present := got["/dec/b"]; present {
		t.Fatalf("/dec/b must have no labels: %#v", got["/dec/b"])
	}

	// A non-labels group is ignored.
	if m, _ := instance.Decorate(ctx, "_other", refs); m != nil {
		t.Fatalf("foreign group must be empty: %#v", m)
	}
	// Empty ref set is empty (not an error).
	if m, err := instance.Decorate(ctx, Group, nil); err != nil || m != nil {
		t.Fatalf("empty refs: %v %#v", err, m)
	}
}

// Groups advertises exactly the "_labels" group.
func TestDecoratorGroups(t *testing.T) {
	g := instance.Groups()
	if len(g) != 1 || g[0] != Group {
		t.Fatalf("groups = %v", g)
	}
}

// Register wires the bundle so a fresh registry can decorate through the F2 host
// path end-to-end.
func TestRegisterEnablesDecoration(t *testing.T) {
	ctx := context.Background()
	mountStorage(t, "/reg")
	l := &model.Label{Name: "reg-tag"}
	if err := op.CreateLabel(l); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteLabel(l.ID) })
	if err := op.ApplyLabel(ctx, "/reg/x", l.ID); err != nil {
		t.Fatal(err)
	}

	r := extension.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatalf("register: %v", err)
	}
	fr := extension.DecorateFields(ctx, r, []string{Group}, []ext.ObjRef{{Anchor: "/reg/x"}}, 0)
	if _, ok := fr.Fields[Group]["/reg/x"]; !ok {
		t.Fatalf("decoration not wired: %+v", fr)
	}
}
