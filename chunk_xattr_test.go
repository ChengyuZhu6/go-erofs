package erofs_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	erofs "github.com/erofs/go-erofs"
	"github.com/erofs/go-erofs/internal/disk"
)

func TestChunkIndex64GiB(t *testing.T) {
	const size = int64(64) << 30
	const block = int64(4096)
	const position = size - 2*block
	device := bytes.Repeat([]byte{0x71}, int(block))
	meta, err := os.Create(filepath.Join(t.TempDir(), "meta.erofs"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = meta.Close() }()
	writer := erofs.Create(meta)
	if err := writer.CopyEntries(erofs.SourceEntries{
		BlockSize: 4096, ExternalDeviceBlocks: []uint64{1},
		Entries: []erofs.SourceEntry{{Path: "/disk.raw", Mode: 0o444, Size: size,
			DataRanges: []erofs.DataRange{{Offset: -1, Size: position},
				{Device: 1, Offset: 0, Size: block, Existing: true}, {Offset: -1, Size: block}}}},
	}, erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := meta.Stat()
	if err != nil || info.Size() < 128<<20 || info.Size() > (128<<20)+65536 {
		t.Fatalf("4KiB index metadata=%v err=%v", info, err)
	}
	image, err := erofs.Open(meta, erofs.WithExtraDevices(bytes.NewReader(device)))
	if err != nil {
		t.Fatal(err)
	}
	file, err := image.Open("disk.raw")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	validated := file.(interface {
		io.ReaderAt
		ValidateData([]int64) error
	})
	if err := validated.ValidateData([]int64{info.Size(), block}); err != nil {
		t.Fatal(err)
	}
	stat, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	want := []erofs.DataRange{{Offset: -1, Size: position},
		{Device: 1, Offset: 0, Size: block}, {Offset: -1, Size: block}}
	if got := stat.(interface{ DataRange() []erofs.DataRange }).DataRange(); !reflect.DeepEqual(got, want) {
		t.Fatalf("64GiB ranges=%+v", got)
	}
	data := make([]byte, 3*block)
	if _, err := validated.ReadAt(data, position-block); err != nil {
		t.Fatal(err)
	}
	expected := append(make([]byte, block), device...)
	expected = append(expected, make([]byte, block)...)
	if !bytes.Equal(data, expected) {
		t.Fatal("4KiB data or adjacent holes changed")
	}
	// Keep actual bytes present while declaring a shorter primary image.
	var declared [4]byte
	binary.LittleEndian.PutUint32(declared[:], 2)
	if _, err := meta.WriteAt(declared[:], disk.SuperBlockOffset+36); err != nil {
		t.Fatal(err)
	}
	corrupt, err := erofs.Open(meta, erofs.WithExtraDevices(bytes.NewReader(device)))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := corrupt.Open("disk.raw")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bad.Close() }()
	if err := bad.(interface{ ValidateData([]int64) error }).ValidateData([]int64{info.Size(), block}); err == nil {
		t.Fatal("chunk index beyond declared primary image accepted")
	}
	t.Logf("CHUNK_INDEX_64GIB_PASS logical=%d metadata=%d ranges=%d", size, info.Size(), len(want))
}

// TestChunkBasedXattrAlignment guards against the chunk-index map being written
// unaligned after the xattr area on a chunk-based (external-device) inode.
//
// The kernel and this package's reader locate the chunk-index map at
// ALIGN(inode_isize + xattr_isize, sizeof(chunk_index)). inode_isize is a
// multiple of 8, so a xattr area sized 4 (mod 8) pushes the map off alignment.
// The "user.t" = "abc" attribute makes xattr_isize == 20, so inode_isize +
// xattr_isize is 4 (mod 8) for both compact (32) and extended (64) inodes;
// before the fix the chunk index was written 4 bytes early and the file
// resolved to the wrong device block.
func TestChunkBasedXattrAlignment(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	var metaBuf testBuffer
	fsys := erofs.Create(&metaBuf, erofs.WithDataFile(df))

	f, err := fsys.Create("/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0xAB}, 4096) // exactly one chunk
	if _, err := f.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Setxattr("/file.bin", "user.t", "abc"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	dfRead, err := os.Open(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dfRead.Close() }()

	efs, err := erofs.Open(bytes.NewReader(metaBuf.Bytes()), erofs.WithExtraDevices(dfRead))
	if err != nil {
		t.Fatal("Open:", err)
	}

	got, err := fs.ReadFile(efs, "file.bin")
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file data mismatch: chunk-index map misaligned after xattr area (got %d bytes, first=0x%02x)", len(got), firstByte(got))
	}

	// The xattr itself should round-trip.
	fi, err := fs.Stat(efs, "file.bin")
	if err != nil {
		t.Fatal("Stat:", err)
	}
	xg, ok := fi.(interface {
		GetXattr(string) (string, bool)
	})
	if !ok {
		t.Fatal("FileInfo does not expose GetXattr")
	}
	if v, ok := xg.GetXattr("user.t"); !ok || v != "abc" {
		t.Fatalf("user.t = %q (ok=%v), want %q", v, ok, "abc")
	}
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

// TestCopyFromImageHoleChunk guards against null chunk indexes being dropped
// while copying metadata from an existing EROFS image.
func TestCopyFromImageHoleChunk(t *testing.T) {
	const blockSize = 4096
	data0 := bytes.Repeat([]byte{0xAA}, blockSize)
	data1 := bytes.Repeat([]byte{0xBB}, blockSize)
	device := bytes.Join([][]byte{
		data0,
		data1,
		bytes.Repeat([]byte{0xCC}, blockSize),
		bytes.Repeat([]byte{0xDD}, blockSize),
	}, nil)
	dstMeta := copyChunkBasedImage(t, erofs.SourceEntry{
		Path: "/sparse.bin",
		Mode: 0o644,
		Size: 4 * blockSize,
		DataRanges: []erofs.DataRange{
			{Offset: -1, Size: blockSize},
			{Device: 0, Offset: 0, Size: blockSize},
			{Offset: -1, Size: blockSize},
			{Device: 0, Offset: blockSize, Size: blockSize},
		},
	}, 4, device)

	dstFS, err := erofs.Open(bytes.NewReader(dstMeta), erofs.WithExtraDevices(bytes.NewReader(device)))
	if err != nil {
		t.Fatal("Open dst:", err)
	}
	got, err := fs.ReadFile(dstFS, "sparse.bin")
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	want := make([]byte, 4*blockSize)
	copy(want[blockSize:2*blockSize], data0)
	copy(want[3*blockSize:], data1)
	if !bytes.Equal(got, want) {
		t.Fatalf("content mismatch after CopyFrom with holes: len got=%d want=%d", len(got), len(want))
	}
}

// TestCopyFromImageLongHoleRun verifies that a hole longer than Chunk.Count can
// represent is split without moving the following data to an earlier offset.
func TestCopyFromImageLongHoleRun(t *testing.T) {
	const (
		blockSize  = int64(4096)
		holeBlocks = int64(1 << 16)
	)
	holeSize := holeBlocks * blockSize
	dstMeta := copyChunkBasedImage(t, erofs.SourceEntry{
		Path: "/long-hole.bin",
		Mode: 0o644,
		Size: holeSize + blockSize,
		DataRanges: []erofs.DataRange{
			{Offset: -1, Size: holeSize},
			{Device: 0, Offset: 0, Size: blockSize},
		},
	}, 1, nil)

	dstFS, err := erofs.Open(bytes.NewReader(dstMeta), erofs.WithExtraDevices(bytes.NewReader(nil)))
	if err != nil {
		t.Fatal("Open dst:", err)
	}
	ranges := statDataRange(t, dstFS, "long-hole.bin")
	if len(ranges) != 2 || ranges[0].Offset != -1 || ranges[0].Size != holeSize ||
		ranges[1].Device != 1 || ranges[1].Offset != 0 || ranges[1].Size != blockSize {
		t.Fatalf("DataRange() = %+v, want %d-byte hole followed by one data block", ranges, holeSize)
	}
}

// TestCopyFromImageContiguousFlag verifies that physically disjoint source
// chunks are not collapsed into one larger chunk in the copied image.
func TestCopyFromImageContiguousFlag(t *testing.T) {
	const blockSize = 4096
	device := make([]byte, 3*blockSize)
	copy(device[:blockSize], bytes.Repeat([]byte{0x11}, blockSize))
	copy(device[blockSize:2*blockSize], bytes.Repeat([]byte{0xEE}, blockSize))
	copy(device[2*blockSize:], bytes.Repeat([]byte{0x22}, blockSize))
	dstMeta := copyChunkBasedImage(t, erofs.SourceEntry{
		Path: "/two.bin",
		Mode: 0o644,
		Size: 2 * blockSize,
		DataRanges: []erofs.DataRange{
			{Device: 0, Offset: 0, Size: blockSize},
			{Device: 0, Offset: 2 * blockSize, Size: blockSize},
		},
	}, 3, device)

	dstFS, err := erofs.Open(bytes.NewReader(dstMeta), erofs.WithExtraDevices(bytes.NewReader(device)))
	if err != nil {
		t.Fatal("Open dst:", err)
	}
	got, err := fs.ReadFile(dstFS, "two.bin")
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	want := append(bytes.Repeat([]byte{0x11}, blockSize), bytes.Repeat([]byte{0x22}, blockSize)...)
	if !bytes.Equal(got, want) {
		t.Fatal("non-contiguous chunks were collapsed")
	}
}

// TestCopyFromImageLargeContiguousRun verifies that internal uint16 Count
// splits do not disable the large-chunk metadata optimization.
func TestCopyFromImageLargeContiguousRun(t *testing.T) {
	const (
		blockSize = int64(4096)
		blocks    = int64(1 << 16)
	)
	dstMeta := copyChunkBasedImage(t, erofs.SourceEntry{
		Path: "/large.bin",
		Mode: 0o644,
		Size: blocks * blockSize,
		DataRanges: []erofs.DataRange{
			{Device: 0, Offset: 0, Size: (blocks - 1) * blockSize},
			{Device: 0, Offset: (blocks - 1) * blockSize, Size: blockSize},
		},
	}, uint64(blocks), nil)

	if len(dstMeta) >= 64*1024 {
		t.Fatalf("copied metadata is %d bytes; contiguous run was not coalesced", len(dstMeta))
	}
}

func copyChunkBasedImage(t *testing.T, entry erofs.SourceEntry, deviceBlocks uint64, device []byte) []byte {
	t.Helper()
	const blockSize = 4096

	var srcMeta testBuffer
	src := erofs.Create(&srcMeta)
	if err := src.CopyEntries(erofs.SourceEntries{
		BlockSize:            blockSize,
		ExternalDeviceBlocks: []uint64{deviceBlocks},
		Entries:              []erofs.SourceEntry{entry},
	}, erofs.MetadataOnly()); err != nil {
		t.Fatal("CopyEntries:", err)
	}
	if err := src.Close(); err != nil {
		t.Fatal("src Close:", err)
	}

	srcFS, err := erofs.Open(bytes.NewReader(srcMeta.Bytes()), erofs.WithExtraDevices(bytes.NewReader(device)))
	if err != nil {
		t.Fatal("Open src:", err)
	}
	var dstMeta testBuffer
	dst := erofs.Create(&dstMeta)
	if err := dst.CopyFrom(srcFS, erofs.MetadataOnly()); err != nil {
		t.Fatal("CopyFrom:", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal("dst Close:", err)
	}
	return append([]byte(nil), dstMeta.Bytes()...)
}
