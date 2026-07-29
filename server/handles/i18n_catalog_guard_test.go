package handles

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/i18n"
)

// This is the red-team I2 regression guard: server/handles has, more than
// once, called common.ErrorI18n or i18n.Error with a literal key that was
// never added to internal/i18n/locales/*.json — the catalog silently falls
// back to rendering the bare key string to the user (i18n.translate's
// "return key" branch), which is how "auth.second_factor_required" and its
// three TOTP siblings shipped as visible-but-untranslated raw keys in Phase C
// before this fix. This test statically scans every non-test .go file in
// this package for such calls and fails loudly if any literal key is
// missing from the "en" catalog (internal/i18n's own TestLocaleParity
// separately guarantees en and zh-CN carry the same key set, so checking
// against "en" here is sufficient — a key present in en but not zh-CN, or
// vice versa, is caught there, not here).
//
// It only checks LITERAL string keys (e.g. ErrorI18n(c, "auth.foo", 400)) —
// a key built from a variable/expression is out of reach for static analysis
// and is silently skipped; none exist in this package today.

// scanI18nCatalogCalls walks every non-test .go file in dir and returns the
// literal keys passed as the key argument to common.ErrorI18n (arg index 1,
// after the *gin.Context) and i18n.Error (arg index 1, after the lang), plus
// the file:line each was found at (for a readable failure message).
func scanI18nCatalogCalls(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	keys := map[string]string{} // key -> "file:line" of first occurrence

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %q: %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			var keyArgIndex int
			switch sel.Sel.Name {
			case "ErrorI18n":
				keyArgIndex = 1 // ErrorI18n(c, key, code, ...)
			case "Error":
				// Scope to i18n.Error specifically — "Error" alone is far too
				// common a method name (error values, etc.) to match blindly.
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "i18n" {
					return true
				}
				keyArgIndex = 1 // i18n.Error(lang, key, err, ...)
			default:
				return true
			}
			if len(call.Args) <= keyArgIndex {
				return true
			}
			lit, ok := call.Args[keyArgIndex].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true // not a literal key — out of reach for static scan
			}
			key, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if _, seen := keys[key]; !seen {
				pos := fset.Position(lit.Pos())
				keys[key] = filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line)
			}
			return true
		})
	}
	return keys
}

// TestEveryI18nLiteralKeyExistsInCatalog is the I2 guard described above.
func TestEveryI18nLiteralKeyExistsInCatalog(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's path")
	}
	dir := filepath.Dir(thisFile)

	keys := scanI18nCatalogCalls(t, dir)
	if len(keys) == 0 {
		t.Fatal("scan found zero ErrorI18n/i18n.Error literal keys — the scan itself is likely broken (this package definitely uses both)")
	}
	var missing []string
	for key, loc := range keys {
		if !i18n.Has("en", key) {
			missing = append(missing, key+" (first seen at "+loc+")")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the following ErrorI18n/i18n.Error literal keys are missing from internal/i18n/locales/en.json:\n  %s",
			strings.Join(missing, "\n  "))
	}
}
