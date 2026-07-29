package thumbnail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/cmd/flags"
	// Import the Local driver directly (init side effect) so op.CreateStorage
	// can back the fixture -- mirrors internal/search/search_test.go.
	_ "github.com/alist-org/alist/v3/drivers/local"
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
	dB, err := gorm.Open(sqlite.Open("file:thumbnail_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}

	// Mirror internal/bootstrap.InitThumbnailers (Phase E wave 3): this
	// package's own Generate tests dispatch through internal/extension now,
	// so the built-in image Thumbnailer must be registered exactly like
	// production bootstrap does, or every Generate call below sees "no
	// thumbnailer claims .png" (ErrNotImageThumbnailable) before ever
	// reaching the image-specific logic under test.
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    ImageThumbnailerExtensionID,
		Kinds: []ext.Kind{ext.KindThumbnailer},
	}); err != nil {
		panic(err)
	}
	extension.RegisterTrustedThumbnailerInstance(ImageThumbnailerExtensionID, NewImageThumbnailer())
}

// ---- pure unit tests: clamp / extension check / resize+encode ----

func TestClampMaxDim(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, DefaultMaxDim},
		{-5, DefaultMaxDim},
		{1, MinMaxDim},
		{MinMaxDim, MinMaxDim},
		{MinMaxDim - 1, MinMaxDim},
		{MaxMaxDim, MaxMaxDim},
		{MaxMaxDim + 1, MaxMaxDim},
		{100000, MaxMaxDim},
		{200, 200},
	}
	for _, c := range cases {
		if got := ClampMaxDim(c.in); got != c.want {
			t.Errorf("ClampMaxDim(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestIsThumbnailableExt(t *testing.T) {
	yes := []string{"a.jpg", "a.JPG", "a.jpeg", "a.png", "a.PNG", "a.gif", "a.webp", "a.bmp", "a.tiff", "/dir/deep/photo.Jpg"}
	no := []string{"a.txt", "a.mp4", "a.pdf", "a", "a.jpg.txt", "a.psd", "a.heic"}
	for _, name := range yes {
		if !isThumbnailableExt(name) {
			t.Errorf("isThumbnailableExt(%q) = false, want true", name)
		}
	}
	for _, name := range no {
		if isThumbnailableExt(name) {
			t.Errorf("isThumbnailableExt(%q) = true, want false", name)
		}
	}
}

// encodePNG builds a solid-color w x h PNG, the synthetic "source image" used
// across these tests.
func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x % 256), G: uint8(y % 256), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	return buf.Bytes()
}

// TestRenderThumbnailResizesAndPreservesAspect: an 800x400 (2:1) source fit
// into maxDim=200 must come out at 200x100 -- both dims <= maxDim, and the
// 2:1 aspect ratio preserved.
func TestRenderThumbnailResizesAndPreservesAspect(t *testing.T) {
	src := encodePNG(t, 800, 400)
	out, err := renderThumbnail(bytes.NewReader(src), 200)
	if err != nil {
		t.Fatalf("renderThumbnail: %v", err)
	}
	img, format, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode thumbnail output: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("thumbnail output format = %q, want jpeg", format)
	}
	b := img.Bounds()
	if b.Dx() > 200 || b.Dy() > 200 {
		t.Fatalf("thumbnail dims %dx%d exceed maxDim 200", b.Dx(), b.Dy())
	}
	if b.Dx() != 200 || b.Dy() != 100 {
		t.Fatalf("thumbnail dims = %dx%d, want 200x100 (2:1 aspect preserved)", b.Dx(), b.Dy())
	}
}

// TestRenderThumbnailNeverUpscales: a source already smaller than maxDim is
// not blown up past its own size (imaging.Fit's contract).
func TestRenderThumbnailNeverUpscales(t *testing.T) {
	src := encodePNG(t, 50, 50)
	out, err := renderThumbnail(bytes.NewReader(src), 200)
	if err != nil {
		t.Fatalf("renderThumbnail: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 50 || b.Dy() != 50 {
		t.Fatalf("thumbnail dims = %dx%d, want unchanged 50x50", b.Dx(), b.Dy())
	}
}

// TestRenderThumbnailDecodeFailure: garbage bytes with no valid image header
// fail closed with ErrDecodeFailed, not a panic or a 500-worthy raw error.
func TestRenderThumbnailDecodeFailure(t *testing.T) {
	_, err := renderThumbnail(bytes.NewReader([]byte("this is not an image, just text")), 200)
	if !errors.Is(err, ErrDecodeFailed) {
		t.Fatalf("renderThumbnail(garbage) error = %v, want ErrDecodeFailed", err)
	}
}

// TestBoundedReaderErrorsPastCap is the unit-level pin on the primitive
// itself: once more than `remaining` bytes have been requested, Read fails
// closed with an explicit error (not a silent EOF/truncation) every time
// after, regardless of what's actually behind it.
func TestBoundedReaderErrorsPastCap(t *testing.T) {
	br := &boundedReader{r: bytes.NewReader(bytes.Repeat([]byte{0xAB}, 1000)), remaining: 10}
	buf := make([]byte, 4)
	var total int
	for {
		n, err := br.Read(buf)
		total += n
		if err != nil {
			if !errors.Is(err, errBoundedReaderCapExceeded) {
				t.Fatalf("Read error = %v, want errBoundedReaderCapExceeded", err)
			}
			break
		}
	}
	if total > 10 {
		t.Fatalf("boundedReader yielded %d bytes, want at most the 10-byte cap", total)
	}
	// Further reads keep failing the same way (fails closed, not just once).
	if _, err := br.Read(buf); !errors.Is(err, errBoundedReaderCapExceeded) {
		t.Fatalf("Read after cap exceeded = %v, want errBoundedReaderCapExceeded again", err)
	}
}

// encodeNoisyPNG builds a w x h PNG of per-pixel random noise -- unlike
// encodePNG's flat color (which zlib compresses to almost nothing), noise is
// close to incompressible, so the encoded byte size reliably exceeds a small
// cap, giving renderThumbnail's bounded reader something real to trip over
// mid-decode rather than failing on the format-sniff of the first few bytes.
func encodeNoisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	next := func() uint8 {
		// xorshift32: fast, deterministic, no math/rand import needed.
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		return uint8(seed)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: next(), G: next(), B: next(), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode noisy fixture png: %v", err)
	}
	return buf.Bytes()
}

// TestRenderThumbnailOversizeSourceRefused: a real (incompressible-enough)
// source that exceeds a deliberately tiny cap is refused with
// ErrSourceTooLarge, the end-to-end (imaging.Decode + boundedReader) path,
// not just the primitive in isolation.
func TestRenderThumbnailOversizeSourceRefused(t *testing.T) {
	src := encodeNoisyPNG(t, 200, 200)

	old := maxThumbSourceBytes
	maxThumbSourceBytes = 200 // comfortably smaller than the noisy fixture
	t.Cleanup(func() { maxThumbSourceBytes = old })

	if int64(len(src)) <= maxThumbSourceBytes {
		t.Fatalf("fixture too small (%d bytes) to exercise the %d-byte cap", len(src), maxThumbSourceBytes)
	}

	_, err := renderThumbnail(bytes.NewReader(src), 200)
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("renderThumbnail(oversize) error = %v, want ErrSourceTooLarge", err)
	}
}

// TestRenderThumbnailOversizePixelsRefused: a tiny source file that declares a
// pixel count above maxThumbPixels is refused BEFORE a full decode allocates the
// bitmap (decompression-bomb guard) -- distinct from the source-byte cap.
func TestRenderThumbnailOversizePixelsRefused(t *testing.T) {
	old := maxThumbPixels
	maxThumbPixels = 100 * 100 // 10k-pixel cap
	t.Cleanup(func() { maxThumbPixels = old })

	src := encodePNG(t, 300, 300) // 90k pixels > cap, but a small file (fits the byte cap)
	if int64(len(src)) > maxThumbSourceBytes {
		t.Fatalf("fixture unexpectedly exceeds the byte cap; this test must exercise the PIXEL cap")
	}
	if _, err := renderThumbnail(bytes.NewReader(src), 64); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("renderThumbnail(over-pixel-cap) error = %v, want ErrSourceTooLarge", err)
	}
}

// TestRenderThumbnailWithinCapDecodesFine: sanity check that lowering the cap
// doesn't itself break decoding of a source that fits comfortably under it.
func TestRenderThumbnailWithinCapDecodesFine(t *testing.T) {
	old := maxThumbSourceBytes
	maxThumbSourceBytes = 1 << 20 // 1 MiB, comfortably above a tiny fixture PNG
	t.Cleanup(func() { maxThumbSourceBytes = old })

	src := encodePNG(t, 40, 40)
	if _, err := renderThumbnail(bytes.NewReader(src), 200); err != nil {
		t.Fatalf("renderThumbnail under cap: %v", err)
	}
}

// ---- integration: Generate() end to end against a real Local storage ----

var thumbFixtureSeq int64

// setupThumbFixture mounts an isolated Local storage containing one real PNG
// (photo.png) and one non-image file (notes.txt), and returns a caller
// scoped to full recursive ActRead on the mount, the mount path, and the
// backing root directory on disk. flags.DataDir is pointed at a throwaway
// dir so the on-disk thumbnail cache never touches a real data directory.
//
// The returned user is built via ScopedPermissions (like
// internal/search/search_test.go's scopedReader), NOT model.Roles{ADMIN}:
// op's policy engine (op.ResolveEffectivePolicy) has no admin short-circuit
// -- a Role-ADMIN user with no seeded role-permission rows in this fixture's
// fresh DB would resolve to zero permissions and every Authorize call would
// deny, silently defeating every "authorized" test case below.
func setupThumbFixture(t *testing.T) (reader *model.User, mount string, rootDir string) {
	t.Helper()
	n := atomic.AddInt64(&thumbFixtureSeq, 1)
	mount = fmt.Sprintf("/thumb%d", n)

	oldDataDir := flags.DataDir
	flags.DataDir = t.TempDir()
	t.Cleanup(func() { flags.DataDir = oldDataDir })

	rootDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "photo.png"), encodePNG(t, 300, 150), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + rootDir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	reader = &model.User{
		ID:       90000 + uint(n),
		Username: fmt.Sprintf("thumbreader%d", n),
		Scoped:   true,
		ScopedPermissions: []model.Permission{{
			Path:      mount,
			Actions:   model.Actions{model.ActRead},
			Effect:    model.EffectAllow,
			Recursive: true,
		}},
	}
	return reader, mount, rootDir
}

// scopedNoAccessReader mirrors setupThumbFixture's reader but with an EMPTY
// policy -- a caller with no grant on the mount at all, used to pin the
// unauthorized-denied case.
func scopedNoAccessReader(id uint) *model.User {
	return &model.User{
		ID:       id,
		Username: fmt.Sprintf("thumbnoaccess%d", id),
		Scoped:   true,
	}
}

func TestGenerateProducesValidJPEGThumbnail(t *testing.T) {
	reader, mount, _ := setupThumbFixture(t)
	path := mount + "/photo.png"

	data, cachedPath, err := Generate(context.Background(), reader, path, 100)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("Generate returned empty data")
	}
	if cachedPath == "" {
		t.Fatal("Generate returned empty cachedPath")
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode generated thumbnail: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("format = %q, want jpeg", format)
	}
	b := img.Bounds()
	if b.Dx() > 100 || b.Dy() > 100 {
		t.Fatalf("dims %dx%d exceed maxDim 100", b.Dx(), b.Dy())
	}
	// 300x150 source is 2:1 -- fit into 100x100 must land at 100x50.
	if b.Dx() != 100 || b.Dy() != 50 {
		t.Fatalf("dims = %dx%d, want 100x50 (aspect preserved)", b.Dx(), b.Dy())
	}
	if _, err := os.Stat(cachedPath); err != nil {
		t.Fatalf("cache file not written to disk: %v", err)
	}
}

// TestGenerateCacheHitSkipsRegeneration: a second call for the same
// path+size returns byte-identical content and does NOT rewrite the cache
// file -- pinned by observing the cache file's mtime is unchanged across the
// second call (a regeneration would rewrite it via the temp+rename path).
func TestGenerateCacheHitSkipsRegeneration(t *testing.T) {
	reader, mount, _ := setupThumbFixture(t)
	path := mount + "/photo.png"

	data1, cachedPath1, err := Generate(context.Background(), reader, path, 100)
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	info1, err := os.Stat(cachedPath1)
	if err != nil {
		t.Fatalf("stat cache after first Generate: %v", err)
	}

	data2, cachedPath2, err := Generate(context.Background(), reader, path, 100)
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if cachedPath1 != cachedPath2 {
		t.Fatalf("cache path changed across identical calls: %q vs %q", cachedPath1, cachedPath2)
	}
	if !bytes.Equal(data1, data2) {
		t.Fatal("cache-hit bytes differ from originally generated bytes")
	}
	info2, err := os.Stat(cachedPath2)
	if err != nil {
		t.Fatalf("stat cache after second Generate: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("cache file mtime changed on a cache-hit call (%v -> %v): it was regenerated, not served from cache",
			info1.ModTime(), info2.ModTime())
	}
}

func TestGenerateNonImageFile(t *testing.T) {
	reader, mount, _ := setupThumbFixture(t)
	_, _, err := Generate(context.Background(), reader, mount+"/notes.txt", 100)
	if !errors.Is(err, ErrNotImageThumbnailable) {
		t.Fatalf("Generate(notes.txt) error = %v, want ErrNotImageThumbnailable", err)
	}
}

func TestGenerateDirectoryIsNotThumbnailable(t *testing.T) {
	reader, mount, _ := setupThumbFixture(t)
	// The mount root itself is a directory (has no image extension either, but
	// pin the IsDir branch specifically in case a directory is ever named
	// "foo.png").
	_, _, err := Generate(context.Background(), reader, mount, 100)
	if !errors.Is(err, ErrNotImageThumbnailable) {
		t.Fatalf("Generate(directory) error = %v, want ErrNotImageThumbnailable", err)
	}
}

func TestGenerateUnauthorizedDenied(t *testing.T) {
	_, mount, _ := setupThumbFixture(t)
	noAccess := scopedNoAccessReader(90500)
	_, _, err := Generate(context.Background(), noAccess, mount+"/photo.png", 100)
	if err == nil {
		t.Fatal("Generate for a caller with no ActRead grant must be denied, got nil error")
	}

	// Same denial for a NON-image path too, and specifically NOT
	// ErrNotImageThumbnailable: Authorize must run before the extension check,
	// so an unauthorized caller can't use the error shape to learn whether a
	// path they can't read is even an image (no oracle on file type).
	_, _, err2 := Generate(context.Background(), noAccess, mount+"/notes.txt", 100)
	if err2 == nil {
		t.Fatal("Generate(notes.txt) for a caller with no ActRead grant must be denied, got nil error")
	}
	if errors.Is(err2, ErrNotImageThumbnailable) {
		t.Fatal("an unauthorized caller must be denied BEFORE the not-an-image check runs (no oracle on file type)")
	}
}

// TestGenerateStaleCacheRegeneratesOnModTimeChange: the cache key folds in
// the object's real ModTime, so touching the backing file (simulating an
// in-place overwrite: same path, later mtime) makes Generate produce a
// DIFFERENT cache entry rather than serving the old thumbnail forever.
func TestGenerateStaleCacheRegeneratesOnModTimeChange(t *testing.T) {
	reader, mount, rootDir := setupThumbFixture(t)
	path := mount + "/photo.png"

	_, cachedPath1, err := Generate(context.Background(), reader, path, 100)
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}

	newer := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(rootDir, "photo.png"), newer, newer); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	_, cachedPath2, err := Generate(context.Background(), reader, path, 100)
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if cachedPath1 == cachedPath2 {
		t.Fatalf("cache path must change once the source ModTime changes, got the same path twice: %q", cachedPath1)
	}
}

func TestCacheFileNameStableAndDistinct(t *testing.T) {
	a := cacheFileName("/x/a.png", 100, 42)
	b := cacheFileName("/x/a.png", 100, 42)
	if a != b {
		t.Fatal("cacheFileName must be deterministic for identical inputs")
	}
	variants := []string{
		cacheFileName("/x/b.png", 100, 42), // different path
		cacheFileName("/x/a.png", 200, 42), // different maxDim
		cacheFileName("/x/a.png", 100, 43), // different modUnix
	}
	for _, v := range variants {
		if v == a {
			t.Fatalf("cacheFileName collided across distinct inputs: %q", a)
		}
	}
}

// TestOpenLinkReaderRejectsEmptyLink pins the defensive fallback: a link with
// neither MFile nor RangeReadCloser nor URL is refused rather than panicking.
func TestOpenLinkReaderRejectsEmptyLink(t *testing.T) {
	_, err := openLinkReader(context.Background(), &model.Link{}, 0)
	if err == nil {
		t.Fatal("openLinkReader on an empty link must error")
	}
}
