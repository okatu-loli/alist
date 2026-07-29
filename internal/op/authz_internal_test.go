package op

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
)

// TestFilterListingFailsClosedOnHideError asserts that a hide-pattern
// resolution error makes filterWithPatterns fail CLOSED (empty listing),
// since Step 5 removes the fs.list hide seam that used to backstop this.
func TestFilterListingFailsClosedOnHideError(t *testing.T) {
	u := &model.User{ID: 3333, Username: "fc", Role: model.Roles{}}
	objs := []model.Obj{&model.Object{Name: "a"}}
	got := filterWithPatterns(u, "/fc", objs, nil, errors.New("db down"))
	if len(got) != 0 {
		t.Fatalf("hide-pattern error must fail closed, got %d objs", len(got))
	}
}
