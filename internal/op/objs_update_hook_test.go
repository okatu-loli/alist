package op

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
)

// TestObjsUpdateHookRegistry proves Register + Handle invokes registered hooks
// with the (parent, objs) pair verbatim, and that a panicking hook is contained
// (recovered) without preventing the remaining hooks from firing.
func TestObjsUpdateHookRegistry(t *testing.T) {
	orig := objsUpdateHooks
	t.Cleanup(func() { objsUpdateHooks = orig })
	objsUpdateHooks = nil

	var gotParent string
	var gotObjs []model.Obj
	fired := false

	// A panicking hook registered FIRST must not abort the observed list nor
	// block the good hook that follows it.
	RegisterObjsUpdateHook(func(parent string, objs []model.Obj) {
		panic("buggy listener")
	})
	RegisterObjsUpdateHook(func(parent string, objs []model.Obj) {
		fired = true
		gotParent = parent
		gotObjs = objs
	})

	objs := []model.Obj{
		&model.Object{Name: "a", IsFolder: true},
		&model.Object{Name: "f.txt", Size: 3},
	}
	HandleObjsUpdateHook("/mnt/dir", objs)

	if !fired {
		t.Fatal("a panicking earlier hook must not prevent later hooks firing")
	}
	if gotParent != "/mnt/dir" {
		t.Fatalf("parent = %q, want /mnt/dir", gotParent)
	}
	if len(gotObjs) != 2 || gotObjs[0].GetName() != "a" || gotObjs[1].GetName() != "f.txt" {
		t.Fatalf("objs = %+v, want [a f.txt] passed through verbatim", gotObjs)
	}
}
