package erofs_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/erofs/go-erofs"
	"github.com/erofs/go-erofs/internal/erofstest"
)

func TestErofs(t *testing.T) {
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}

	minChunk := os.Getpagesize()

	for _, tc := range []struct {
		name  string
		test  erofstest.TestCase
		flags []string // extra mkfs.erofs flags
	}{
		{"Basic", erofstest.Basic, nil},
		{"FileSizes", erofstest.FileSizes, nil},
		{"UIDGIDValues", erofstest.UIDGIDValues, nil},
		{"LongXattrs", erofstest.LongXattrs, erofstest.XattrPrefixFlags()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, cc := range []struct {
				name string
				conv erofstest.Converter
			}{
				{"default", erofstest.MkfsErofs(tc.flags...)},
				{fmt.Sprintf("chunk-%d", minChunk), erofstest.MkfsErofs(append(tc.flags, fmt.Sprintf("--chunksize=%d", minChunk))...)},
				{fmt.Sprintf("chunk-%d", minChunk*2), erofstest.MkfsErofs(append(tc.flags, fmt.Sprintf("--chunksize=%d", minChunk*2))...)},
				{"chunk-index", erofstest.MkfsErofsBlobDev(minChunk, tc.flags...)},
			} {
				t.Run(cc.name, func(t *testing.T) {
					tc.test.Run(t, cc.conv)
				})
			}
		})
	}

	// Large file: 256MB+ to exercise chunk index overflow (run once with 4K chunks).
	t.Run("LargeFile", func(t *testing.T) {
		erofstest.LargeFile.Run(t, erofstest.MkfsErofs(fmt.Sprintf("--chunksize=%d", minChunk)))
	})

	// Sparse files require --chunksize and produce images under 1MB
	// despite 30MB of logical content.
	t.Run("SparseFiles", func(t *testing.T) {
		chunkFlag := fmt.Sprintf("--chunksize=%d", minChunk)
		erofstest.SparseFiles.Run(t, erofstest.MkfsErofsMaxSize(1024*1024, chunkFlag))
	})

	t.Run("lz4", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("mkfs.erofs compression is not included on Windows")
		}
		tc := erofstest.TarContext{}
		wt := erofstest.TarAll(
			tc.File("/file.txt", []byte("content\n"), 0644),
		)
		tarStream := erofstest.TarFromWriterTo(wt)
		defer func() {
			if err := tarStream.Close(); err != nil {
				t.Fatal(err)
			}
		}()

		path := filepath.Join(t.TempDir(), "compressed.erofs")
		if err := erofstest.ConvertTarErofs(context.Background(), tarStream, path, "", []string{"-zlz4"}); err != nil {
			t.Fatal(err)
		}

		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}()

		image, err := erofs.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := fs.ReadFile(image, "file.txt"); err != nil || string(got) != "content\n" {
			t.Fatalf("LZ4 content: %q %v", got, err)
		}
	})
}

type rejectCompressedPayload struct {
	reads int
}

func (r *rejectCompressedPayload) ReadAt([]byte, int64) (int, error) {
	r.reads++
	return 0, errors.New("unexpected payload read")
}

func TestCompressedWriter(t *testing.T) {
	for _, external := range []bool{false, true} {
		root := t.TempDir()
		meta, err := os.Create(filepath.Join(root, "meta.erofs"))
		if err != nil {
			t.Fatal(err)
		}
		defer meta.Close()
		blob, err := os.Create(filepath.Join(root, "blob.erofs"))
		if err != nil {
			t.Fatal(err)
		}
		defer blob.Close()
		options := []erofs.CreateOpt{erofs.WithCompression(), erofs.WithBuildTime(0, 0)}
		if external {
			options = append(options, erofs.WithDataFile(blob))
		}
		large := bytes.Repeat([]byte("pure Go LZ4 disk block"), 65536)
		plain := make([]byte, 8193)
		state := uint32(0x13579bdf)
		for i := range plain {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			plain[i] = byte(state)
		}
		writer := erofs.Create(meta, options...)
		if err := writer.CopyEntries(erofs.SourceEntries{Entries: []erofs.SourceEntry{
			{Path: "/large", Mode: 0644, Size: int64(len(large)), Data: bytes.NewReader(large)},
			{Path: "/plain", Mode: 0644, Size: int64(len(plain)), Data: bytes.NewReader(plain)},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		var readOptions []erofs.OpenOpt
		if external {
			readOptions = append(readOptions, erofs.WithExtraDevices(blob))
		}
		image, err := erofs.Open(meta, readOptions...)
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string][]byte{"large": large, "plain": plain} {
			got, err := fs.ReadFile(image, name)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("compressed writer external=%t %s: %v bytes=%d", external, name, err, len(got))
			}
		}
		info, err := blob.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if external && info.Size() >= int64(len(large))/2 {
			t.Fatal("compressed writer did not reduce payload")
		}
		if external {
			denied := &rejectCompressedPayload{}
			parent, err := erofs.Open(meta, erofs.WithExtraDevices(denied))
			if err != nil {
				t.Fatal(err)
			}
			childMeta, err := os.Create(filepath.Join(root, "child.erofs"))
			if err != nil {
				t.Fatal(err)
			}
			defer childMeta.Close()
			childBlob, err := os.Create(filepath.Join(root, "child.blob"))
			if err != nil {
				t.Fatal(err)
			}
			defer childBlob.Close()
			child := erofs.Create(childMeta, erofs.WithCompression(), erofs.WithDataFile(childBlob), erofs.WithBuildTime(0, 0), erofs.WithHotFiles("new"))
			if err := child.CopyFrom(parent, erofs.MetadataOnly()); err != nil {
				t.Fatalf("compressed inheritance: %v", err)
			}
			if denied.reads != 0 {
				t.Fatal("compressed inheritance read parent payload")
			}
			newFile, err := child.Create("new")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := newFile.Write(plain); err != nil {
				t.Fatal(err)
			}
			if err := newFile.Close(); err != nil {
				t.Fatal(err)
			}
			if got, err := fs.ReadFile(child, "new"); err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("compression spool readback: %v", err)
			}
			if err := child.Close(); err != nil {
				t.Fatal(err)
			}
			if child.HotDataEnd() <= 0 {
				t.Fatal("hot data prefix was not recorded")
			}
			decoded, err := erofs.Open(childMeta, erofs.WithExtraDevices(blob, childBlob))
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string][]byte{"large": large, "plain": plain, "new": plain} {
				got, err := fs.ReadFile(decoded, name)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("inherited compressed file %s: %v", name, err)
				}
			}
		}
		if _, err := erofstest.CheckMkfsVersion("1.0"); err == nil {
			args := []string{"--extract=" + filepath.Join(root, "extracted")}
			oraclePath := meta.Name()
			if external {
				// Stock erofs-utils 1.9.4 map_dev subtracts uniaddr without selecting
				// the device. Flatten its address space only for this decoder oracle.
				primary, err := os.ReadFile(meta.Name())
				if err != nil {
					t.Fatal(err)
				}
				payload, err := os.ReadFile(blob.Name())
				if err != nil {
					t.Fatal(err)
				}
				binary.LittleEndian.PutUint32(primary[1060:1064], uint32((len(primary)+len(payload))/4096))
				binary.LittleEndian.PutUint32(primary[1104:1108], binary.LittleEndian.Uint32(primary[1104:1108])&^uint32(8))
				binary.LittleEndian.PutUint16(primary[1110:1112], 0)
				oraclePath = filepath.Join(root, "unified-reference.erofs")
				if err := os.WriteFile(oraclePath, append(primary, payload...), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args = append(args, oraclePath)
			if output, err := exec.Command("fsck.erofs", args...).CombinedOutput(); err != nil {
				t.Fatalf("independent compressed writer fsck: %v\n%s", err, output)
			}
			got, err := os.ReadFile(filepath.Join(root, "extracted/large"))
			if err != nil || !bytes.Equal(got, large) {
				t.Fatalf("fsck output: %v", err)
			}
		}
	}
}

func TestOpenCompressedMetadata(t *testing.T) {
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}
	for _, tc := range []struct {
		name   string
		flags  []string
		layout uint8
	}{
		{"compact", []string{"-Eforce-inode-compact"}, 3},
		{"extended", []string{"-Eforce-inode-extended,legacy-compress"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := erofstest.TarContext{UID: 123, GID: 456, Xattrs: map[string]string{"user.test": "value"}}
			large := bytes.Repeat([]byte("compressed payload"), 8192)
			stream := erofstest.TarFromWriterTo(erofstest.TarAll(
				ctx.Dir("/dir", 0755), ctx.File("/dir/large", large, 0640),
				ctx.File("/small", []byte("small payload"), 0644),
				ctx.Link("dir/large", "/hardlink"), ctx.Symlink("dir/large", "/symlink"),
			))
			defer func() { _ = stream.Close() }()
			root := t.TempDir()
			metaPath, dataPath := filepath.Join(root, "meta.erofs"), filepath.Join(root, "data.erofs")
			flags := append([]string{"-zlz4", "--tar=f", "--blobdev=" + dataPath, "-Enoinline_data"}, tc.flags...)
			if err := erofstest.ConvertTarErofs(context.Background(), stream, metaPath, "", flags); err != nil {
				if bytes.Contains([]byte(err.Error()), []byte("--blobdev must be used together with --chunksize")) {
					t.Skip("mkfs.erofs lacks compressed external-device support")
				}
				t.Fatal(err)
			}
			raw, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatal(err)
			}
			denied := &rejectCompressedPayload{}
			if _, err := erofs.Open(bytes.NewReader(raw), erofs.WithExtraDevices(denied)); err != nil {
				t.Fatalf("default Open must accept supported compression without reading payload: %v", err)
			}
			image, err := erofs.Open(bytes.NewReader(raw), erofs.WithExtraDevices(denied), erofs.WithCompressedMetadata())
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.WalkDir(image, ".", func(name string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				_, err = entry.Info()
				return err
			}); err != nil {
				t.Fatal(err)
			}
			info, err := fs.Stat(image, "dir/large")
			if err != nil {
				t.Fatal(err)
			}
			stat := info.Sys().(*erofs.Stat)
			if info.Size() != int64(len(large)) || info.Mode().Perm() != 0640 || stat.UID != 123 || stat.GID != 456 || stat.InodeLayout != tc.layout || stat.Xattrs["user.test"] != "value" {
				t.Fatalf("compressed metadata mismatch: size=%d mode=%v stat=%+v", info.Size(), info.Mode(), stat)
			}
			if ranges := info.(interface{ DataRange() []erofs.DataRange }).DataRange(); ranges != nil {
				t.Fatalf("compressed DataRange must not expose decoded bytes as physical extents: %+v", ranges)
			}
			link, err := fs.Stat(image, "hardlink")
			if err != nil || link.Sys().(*erofs.Stat).Ino != stat.Ino {
				t.Fatalf("hardlink metadata mismatch: %v", err)
			}
			for _, name := range []string{"dir/large", "symlink"} {
				file, err := image.Open(name)
				if err != nil {
					t.Fatal(err)
				}
				_, readErr := file.Read(make([]byte, 1))
				_ = file.Close()
				if !errors.Is(readErr, erofs.ErrNotImplemented) {
					t.Fatalf("compressed Read %s must reject data access: %v", name, readErr)
				}
				if _, err := fs.ReadFile(image, name); !errors.Is(err, erofs.ErrNotImplemented) {
					t.Fatalf("compressed ReadFile %s must reject data access: %v", name, err)
				}
			}
			if denied.reads != 0 {
				t.Fatalf("metadata operations read external payload %d times", denied.reads)
			}
			data, err := os.Open(dataPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = data.Close() }()
			image, err = erofs.Open(bytes.NewReader(raw), erofs.WithExtraDevices(data), erofs.WithCompressedMetadata())
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := erofs.Open(bytes.NewReader(raw), erofs.WithExtraDevices(data))
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"dir/large", "hardlink", "symlink"} {
				got, err := fs.ReadFile(decoded, name)
				if err != nil || !bytes.Equal(got, large) {
					t.Fatalf("pure Go compressed read %s: %v, bytes=%d", name, err, len(got))
				}
				file, err := decoded.Open(name)
				if err != nil {
					t.Fatal(err)
				}
				for _, off := range []int64{0, 4090, 65530, int64(len(large) - 37)} {
					p := make([]byte, 128)
					n, err := file.(io.ReaderAt).ReadAt(p, off)
					want := min(len(p), len(large)-int(off))
					if n != want || !bytes.Equal(p[:n], large[off:off+int64(n)]) || (want == len(p) && err != nil) || (want < len(p) && !errors.Is(err, io.EOF)) {
						t.Fatalf("compressed random read %s@%d: n=%d err=%v", name, off, n, err)
					}
				}
				_ = file.Close()
			}
			if got, err := fs.ReadFile(image, "small"); err != nil || string(got) != "small payload" {
				t.Fatalf("uncompressed fallback unreadable: %q, %v", got, err)
			}
			binary.LittleEndian.PutUint32(raw[1104:1108], binary.LittleEndian.Uint32(raw[1104:1108])|0x80000000)
			if _, err := erofs.Open(bytes.NewReader(raw), erofs.WithExtraDevices(denied), erofs.WithCompressedMetadata()); !errors.Is(err, erofs.ErrNotImplemented) {
				t.Fatalf("metadata option must not allow unknown features: %v", err)
			}
		})
	}
}

func TestOpenTarIndexWithoutCompression(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfs.erofs tar-index mode is not included on Windows")
	}
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "data.tar")
	tarFile, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tarStream := erofstest.TarFromWriterTo(erofstest.TarAll(
		erofstest.TarContext{}.File("/file.txt", []byte("content\n"), 0644),
	))
	if _, err := io.Copy(tarFile, tarStream); err != nil {
		t.Fatal(err)
	}
	if err := tarStream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tarFile.Close(); err != nil {
		t.Fatal(err)
	}

	imagePath := filepath.Join(dir, "meta.erofs")
	if out, err := exec.Command("mkfs.erofs", "--tar=i", imagePath, tarPath).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.erofs --tar=i: %v\n%s", err, out)
	}
	imageFile, err := os.Open(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = imageFile.Close() })
	dataFile, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataFile.Close() })

	image, err := erofs.Open(imageFile, erofs.WithExtraDevices(dataFile))
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(image, "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content\n" {
		t.Fatalf("file content=%q, want %q", data, "content\n")
	}
}

func TestCopyFromTarIndexLargeDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfs.erofs tar-index mode is not included on Windows")
	}
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "data.tar")
	tarFile, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	entries := make(chan erofstest.WriterToTar)
	go func() {
		entries <- erofstest.TarContext{}.Dir("/many", 0o755)
		for i := range 5000 {
			entries <- erofstest.TarContext{}.File(fmt.Sprintf("/many/file-%d.txt", i+1), []byte("content\n"), 0o644)
		}
		close(entries)
	}()
	tarStream := erofstest.TarFromWriterTo(erofstest.TarAll(erofstest.TarStream(entries)))
	if _, err := io.Copy(tarFile, tarStream); err != nil {
		t.Fatal(err)
	}
	if err := tarStream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tarFile.Close(); err != nil {
		t.Fatal(err)
	}

	imagePath := filepath.Join(dir, "meta.erofs")
	if out, err := exec.Command("mkfs.erofs", "--tar=i", imagePath, tarPath).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.erofs --tar=i: %v\n%s", err, out)
	}
	imageFile, err := os.Open(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = imageFile.Close() }()
	dataFile, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dataFile.Close() }()
	base, err := erofs.Open(imageFile, erofs.WithExtraDevices(dataFile))
	if err != nil {
		t.Fatal("open base:", err)
	}

	var copied testBuffer
	writer := erofs.Create(&copied)
	if err := writer.CopyFrom(base, erofs.MetadataOnly()); err != nil {
		t.Fatal("copy base:", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal("close copied image:", err)
	}
	result, err := erofs.Open(bytes.NewReader(copied.Bytes()), erofs.WithExtraDevices(dataFile))
	if err != nil {
		t.Fatal("open copied image:", err)
	}
	for _, name := range []string{"many/file-1.txt", "many/file-2500.txt", "many/file-5000.txt"} {
		if _, err := fs.Stat(result, name); err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
}

func BenchmarkLookup(b *testing.B) {
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		b.Skipf("skipping: %v", err)
	}

	tc := erofstest.TarContext{}.WithModTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	// Build a tar with directories of varying sizes.
	lotsOfFiles := make(chan erofstest.WriterToTar)
	go func() {
		for i := range 5000 {
			lotsOfFiles <- tc.File(fmt.Sprintf("/bigdir/%d", i), []byte{}, 0600)
		}
		close(lotsOfFiles)
	}()

	wt := erofstest.TarAll(
		tc.Dir("/a", 0755),
		tc.Dir("/a/b", 0755),
		tc.Dir("/a/b/c", 0755),
		tc.File("/a/b/c/file.txt", []byte("content\n"), 0644),
		tc.Dir("/smalldir", 0755),
		tc.File("/smalldir/file.txt", []byte("content\n"), 0644),
		tc.Dir("/bigdir", 0755),
		erofstest.TarStream(lotsOfFiles),
	)

	fsys := erofstest.MkfsErofs()(b, wt)

	for _, bc := range []struct {
		name string
		path string
	}{
		{"shallow", "smalldir/file.txt"},
		{"deep", "a/b/c/file.txt"},
		{"bigdir-first", "bigdir/0"},
		{"bigdir-last", "bigdir/4999"},
		{"bigdir-notfound", "bigdir/nonexistent"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			for range b.N {
				f, err := fsys.Open(bc.path)
				if err != nil {
					if !errors.Is(err, fs.ErrNotExist) {
						b.Fatal(err)
					}
					continue
				}
				_ = f.Close()
			}
		})
	}
}

// dataRanger is used for type-asserting DataRange() from fs.FileInfo.
type dataRanger interface {
	DataRange() []erofs.DataRange
}

// checkDataRangeCoverage asserts that the DataRange() slices returned by
// Stat() on name cover exactly fileSize bytes, with all positive sizes.
func checkDataRangeCoverage(t *testing.T, fsys fs.FS, name string, fileSize int) {
	t.Helper()
	f, err := fsys.Open(name)
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat %s: %v", name, err)
	}
	if info.Size() != int64(fileSize) {
		t.Fatalf("Size = %d, want %d", info.Size(), fileSize)
	}
	// All erofs.fileInfo values implement DataRange(); no type-assert guard needed.
	dr := info.(dataRanger)
	ranges := dr.DataRange()
	if len(ranges) == 0 {
		t.Fatalf("DataRange() returned nil for non-empty file %s", name)
	}
	var total int64
	for _, r := range ranges {
		if r.Size <= 0 {
			t.Errorf("range has non-positive Size %d: %+v", r.Size, r)
		}
		total += r.Size
	}
	if total != info.Size() {
		t.Errorf("total DataRange size = %d, want %d; ranges = %+v", total, info.Size(), ranges)
	}
}

// TestDataRangeMultiBlockCoverage verifies that DataRange() total size equals
// the file size for multi-block files across the layouts mkfs.erofs produces.
//
// The tar pipeline (all platforms) exercises FlatPlain. The dir-source
// subtest (non-Windows only) exercises FlatInline multi-block, which is the
// layout that contained the headSize bug (inodeData treated as block count
// instead of block address).
func TestDataRangeMultiBlockCoverage(t *testing.T) {
	if _, err := erofstest.CheckMkfsVersion("1.0"); err != nil {
		t.Skipf("skipping: %v", err)
	}

	const blockSize = 4096

	sizes := []struct {
		name     string
		fileSize int
	}{
		{"2-block", blockSize + 100},
		{"3-block", blockSize*2 + 200},
		{"4-block", blockSize*3 + 50},
	}

	// tar pipeline: works on all platforms; mkfs.erofs uses -Enoinline_data
	// so files land in FlatPlain layout (single contiguous range).
	t.Run("tar-pipeline", func(t *testing.T) {
		for _, tc := range sizes {
			t.Run(tc.name, func(t *testing.T) {
				content := bytes.Repeat([]byte("x"), tc.fileSize)
				tarCtx := erofstest.TarContext{}
				wt := erofstest.TarAll(tarCtx.File("/file.bin", content, 0o644))
				fsys := erofstest.MkfsErofs()(t, wt)
				checkDataRangeCoverage(t, fsys, "file.bin", tc.fileSize)
			})
		}
	})

	// dir-source pipeline: mkfs.erofs with a directory argument produces
	// FlatInline layout for files whose tail fits in the inode block, giving
	// multi-block FlatInline files. This is what the headSize fix targets.
	// Skipped on Windows because the cross-compiled mkfs.erofs there only
	// supports --tar input.
	t.Run("dir-source-flatinline", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("mkfs.erofs directory-source mode not available on Windows")
		}
		for _, tc := range sizes {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				content := bytes.Repeat([]byte("x"), tc.fileSize)
				if err := os.WriteFile(filepath.Join(dir, "file.bin"), content, 0o644); err != nil {
					t.Fatal(err)
				}
				imgPath := filepath.Join(t.TempDir(), "test.erofs")
				out, err := exec.Command("mkfs.erofs", imgPath, dir).CombinedOutput()
				if err != nil {
					t.Fatalf("mkfs.erofs: %s: %v", out, err)
				}
				imgData, err := os.ReadFile(imgPath)
				if err != nil {
					t.Fatal(err)
				}
				fsys, err := erofs.Open(bytes.NewReader(imgData))
				if err != nil {
					t.Fatal("Open:", err)
				}
				checkDataRangeCoverage(t, fsys, "file.bin", tc.fileSize)
			})
		}
	})
}

// TestDataRangeSparseChunkBased verifies that DataRange() correctly represents
// sparse (hole-containing) chunk-based EROFS files.  It uses the existing
// testdata fixture which contains known sparse files.
//
// Invariants checked:
//   - sum(Size) == info.Size() for every regular file.
//   - Hole entries have Offset == -1.
//   - At least one file in the fixture has a hole so we are actually
//     exercising the hole-emission path in buildChunkDataRanges.
func TestDataRangeSparseChunkBased(t *testing.T) {
	// basic-chunk-index.erofs stores file data on a separate blob device.
	metaData, err := os.ReadFile("testdata/basic-chunk-index.erofs")
	if err != nil {
		t.Skipf("skipping: testdata not available: %v", err)
	}
	blobData, err := os.ReadFile("testdata/basic-chunk-index-data.erofs")
	if err != nil {
		t.Skipf("skipping: testdata blob not available: %v", err)
	}

	fsys, err := erofs.Open(bytes.NewReader(metaData),
		erofs.WithExtraDevices(bytes.NewReader(blobData)))
	if err != nil {
		t.Fatal("Open:", err)
	}

	foundHole := false
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := fsys.Open(p)
		if err != nil {
			return fmt.Errorf("Open %s: %w", p, err)
		}
		defer func() { _ = f.Close() }()

		info, err := f.Stat()
		if err != nil {
			return fmt.Errorf("Stat %s: %w", p, err)
		}

		dr := info.(dataRanger)
		ranges := dr.DataRange()

		// Every non-empty regular file must have at least one range.
		if info.Size() > 0 && len(ranges) == 0 {
			t.Errorf("%s: Size=%d but DataRange() returned nil", p, info.Size())
			return nil
		}

		// Invariant: sum(Size) == file size.
		var total int64
		for j, r := range ranges {
			if r.Size <= 0 {
				t.Errorf("%s: DataRange[%d].Size = %d, want > 0", p, j, r.Size)
			}
			total += r.Size
			if r.Offset == -1 {
				foundHole = true
			}
		}
		if total != info.Size() {
			t.Errorf("%s: sum(DataRange.Size) = %d, want %d", p, total, info.Size())
		}
		return nil
	})
	if err != nil {
		t.Fatal("WalkDir:", err)
	}

	if !foundHole {
		t.Error("no hole entries found in fixture — buildChunkDataRanges hole path was not exercised")
	}
}
