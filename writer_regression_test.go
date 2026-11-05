package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"testing"

	"github.com/erofs/go-erofs"
)

func TestWriterLZ4ConfigActualMaximum(t *testing.T) {
	for _, blockSize := range []int{512, 4096} {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("block=%d/external=%t", blockSize, external), func(t *testing.T) {
				meta, err := os.CreateTemp(t.TempDir(), "meta-")
				if err != nil {
					t.Fatal(err)
				}
				defer meta.Close()
				opts := []erofs.CreateOpt{erofs.WithBlockSize(blockSize), erofs.WithCompression()}
				var readOpts []erofs.OpenOpt
				if external {
					blob, err := os.CreateTemp(t.TempDir(), "blob-")
					if err != nil {
						t.Fatal(err)
					}
					defer blob.Close()
					opts = append(opts, erofs.WithDataFile(blob))
					readOpts = append(readOpts, erofs.WithExtraDevices(blob))
				}
				payload := bytes.Repeat([]byte("compress-me"), 18000)
				writer := erofs.Create(meta, opts...)
				if err := writer.CopyEntries(erofs.SourceEntries{Entries: []erofs.SourceEntry{{Path: "/file", Mode: 0o444, Size: int64(len(payload)), Data: bytes.NewReader(payload)}}}); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				image, err := erofs.Open(meta, readOpts...)
				if err != nil {
					t.Fatal(err)
				}
				info, err := fs.Stat(image, "file")
				if err != nil {
					t.Fatal(err)
				}
				ranges, err := info.(interface {
					EncodedRanges() ([]erofs.EncodedRange, error)
				}).EncodedRanges()
				if err != nil {
					t.Fatal(err)
				}
				maxBlocks := int64(1)
				for _, r := range ranges {
					if r.Algorithm == 0 {
						maxBlocks = max(maxBlocks, r.EncodedSize/int64(blockSize))
					}
				}
				var header [2]byte
				if _, err := meta.ReadAt(header[:], 1156); err != nil {
					t.Fatal(err)
				}
				if got := binary.LittleEndian.Uint16(header[:]); int64(got) != maxBlocks || got > 256 {
					t.Fatalf("LZ4 max_pclusterblks=%d, actual maximum=%d (4KiB-page limit=256)", got, maxBlocks)
				}
			})
		}
	}
	for _, blocks := range []int64{3, 7, 256, 257} {
		t.Run(fmt.Sprintf("inherited=%d", blocks), func(t *testing.T) {
			meta, err := os.CreateTemp(t.TempDir(), "meta-")
			if err != nil {
				t.Fatal(err)
			}
			defer meta.Close()
			writer := erofs.Create(meta, erofs.WithBlockSize(512))
			if err := writer.CopyEntries(erofs.SourceEntries{
				BlockSize: 512, ExternalDeviceBlocks: []uint64{512},
				Entries: []erofs.SourceEntry{
					{Path: "/lz4", Mode: 0o444, Size: 8192, EncodedRanges: []erofs.EncodedRange{{Device: 1, Size: 8192, EncodedSize: blocks * 512}}},
					{Path: "/plain", Mode: 0o444, Size: 8192, EncodedRanges: []erofs.EncodedRange{{Device: 1, Size: 8192, EncodedSize: 8192, Algorithm: 254}}},
				},
			}, erofs.MetadataOnly()); err != nil {
				t.Fatal(err)
			}
			err = writer.Close()
			if blocks > 256 {
				if !errors.Is(err, erofs.ErrNotImplemented) {
					t.Fatalf("LZ4 kernel maximum: got %v, want ErrNotImplemented", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var header [2]byte
			if _, err := meta.ReadAt(header[:], 1156); err != nil {
				t.Fatal(err)
			}
			if got := binary.LittleEndian.Uint16(header[:]); int64(got) != blocks {
				t.Fatalf("inherited LZ4 max=%d, want %d; plain ranges must not inflate it", got, blocks)
			}
		})
	}
}

func TestWriterSubpageChunkBoundaries(t *testing.T) {
	for _, hole := range []bool{true, false} {
		t.Run(fmt.Sprintf("hole=%t", hole), func(t *testing.T) {
			meta, err := os.CreateTemp(t.TempDir(), "meta-")
			if err != nil {
				t.Fatal(err)
			}
			defer meta.Close()
			first := bytes.Repeat([]byte("a"), 4096)
			second := bytes.Repeat([]byte("b"), 4096)
			ranges := []erofs.DataRange{{Device: 0, Offset: 0, Size: 512}, {Device: 1, Offset: 0, Size: 3584}}
			want := append(append([]byte(nil), first[:512]...), second[:3584]...)
			if hole {
				ranges[1] = erofs.DataRange{Offset: -1, Size: 3584}
				clear(want[512:])
			}
			writer := erofs.Create(meta, erofs.WithBlockSize(512))
			if err := writer.CopyEntries(erofs.SourceEntries{
				BlockSize: 512, ExternalDeviceBlocks: []uint64{8, 8},
				Entries: []erofs.SourceEntry{{Path: "/file", Mode: 0o444, Size: 4096, DataRanges: ranges}},
			}, erofs.MetadataOnly()); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			image, err := erofs.Open(meta, erofs.WithExtraDevices(bytes.NewReader(first), bytes.NewReader(second)))
			if err != nil {
				t.Fatal(err)
			}
			got, err := fs.ReadFile(image, "file")
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("512-byte mapping boundary lost: err=%v bytes=%d", err, len(got))
			}
			info, err := fs.Stat(image, "file")
			if err != nil {
				t.Fatal(err)
			}
			mapped := info.(interface{ DataRange() []erofs.DataRange }).DataRange()
			if len(mapped) != 2 || mapped[0].Size != 512 || mapped[1].Size != 3584 {
				t.Fatalf("512-byte mapping split: %v", mapped)
			}
		})
	}
}

func TestWriterHotDataPrefix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		compressed bool
		external   bool
		hotSize    int
		alias      bool
	}{
		{"compressed_image", true, false, 65536, false},
		{"compressed_external", true, true, 65536, false},
		{"plain_image", false, false, 65536, false},
		{"small_image", false, false, 3, false},
		{"hot_alias", false, false, 65536, true},
		{"compressed_hot_alias", true, false, 65536, true},
		{"external_hot_alias", true, true, 65536, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta, err := os.CreateTemp(t.TempDir(), "meta-")
			if err != nil {
				t.Fatal(err)
			}
			defer meta.Close()
			hotPath := "z-hot"
			if tc.alias {
				hotPath = "zz-alias"
			}
			options := []erofs.CreateOpt{erofs.WithBlockSize(4096), erofs.WithBuildTime(0, 0), erofs.WithHotFiles(hotPath)}
			if tc.compressed {
				options = append(options, erofs.WithCompression())
			}
			var readOptions []erofs.OpenOpt
			if tc.external {
				blob, err := os.CreateTemp(t.TempDir(), "blob-")
				if err != nil {
					t.Fatal(err)
				}
				defer blob.Close()
				options = append(options, erofs.WithDataFile(blob))
				readOptions = append(readOptions, erofs.WithExtraDevices(blob))
			}
			cold := bytes.Repeat([]byte("c"), 65536)
			hot := bytes.Repeat([]byte("h"), tc.hotSize)
			writer := erofs.Create(meta, options...)
			entries := []erofs.SourceEntry{
				{Path: "/a-cold", Mode: 0o444, Size: int64(len(cold)), Data: bytes.NewReader(cold)},
				{Path: "/z-hot", Mode: 0o444, Size: int64(len(hot)), Data: bytes.NewReader(hot)},
			}
			if tc.alias {
				entries[1].HardlinkKey = "hot-group"
				entries = append(entries, erofs.SourceEntry{Path: "/zz-alias", Mode: 0o444, Size: int64(len(hot)), HardlinkKey: "hot-group"})
			}
			if err := writer.CopyEntries(erofs.SourceEntries{Entries: entries}); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			image, err := erofs.Open(meta, readOptions...)
			if err != nil {
				t.Fatal(err)
			}
			var hotOffset, hotEnd, coldOffset int64
			var coldIno, hotIno uint64
			for name, want := range map[string][]byte{"a-cold": cold, "z-hot": hot} {
				got, err := fs.ReadFile(image, name)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s readback: %v, bytes=%d", name, err, len(got))
				}
				info, err := fs.Stat(image, name)
				if err != nil {
					t.Fatal(err)
				}
				var offset, stored int64
				if tc.compressed {
					ranges, err := info.(interface {
						EncodedRanges() ([]erofs.EncodedRange, error)
					}).EncodedRanges()
					if err != nil || len(ranges) != 1 {
						t.Fatalf("%s encoded ranges: %v %v", name, ranges, err)
					}
					offset, stored = ranges[0].Offset, ranges[0].EncodedSize
				} else {
					ranges := info.(interface{ DataRange() []erofs.DataRange }).DataRange()
					if len(ranges) != 1 {
						t.Fatalf("%s data ranges: %v", name, ranges)
					}
					offset, stored = ranges[0].Offset, (ranges[0].Size+4095)/4096*4096
				}
				ino := info.(interface{ Ino() uint64 }).Ino()
				if name == "z-hot" {
					hotOffset, hotEnd, hotIno = offset, offset+stored, ino
				} else {
					coldOffset, coldIno = offset, ino
				}
			}
			wantStart := int64(4096)
			if tc.external {
				wantStart = 0
			}
			if hotOffset != wantStart || coldOffset < hotEnd || writer.HotDataEnd() != hotEnd {
				t.Fatalf("hot prefix: hot=%d..%d cold=%d HotDataEnd=%d, want start=%d", hotOffset, hotEnd, coldOffset, writer.HotDataEnd(), wantStart)
			}
			if coldIno >= hotIno {
				t.Fatalf("hot data sorting reordered metadata: cold inode=%d hot inode=%d", coldIno, hotIno)
			}
			if tc.alias {
				alias, err := fs.Stat(image, hotPath)
				if err != nil || alias.(interface{ Ino() uint64 }).Ino() != hotIno {
					t.Fatalf("hot alias lost canonical inode: %v %v", alias, err)
				}
				got, err := fs.ReadFile(image, hotPath)
				if err != nil || !bytes.Equal(got, hot) {
					t.Fatalf("hot alias readback: %v, bytes=%d", err, len(got))
				}
			}
		})
	}
}

func BenchmarkWriterCompressedMetadata(b *testing.B) {
	for _, count := range []int{128, 1024} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			meta, err := os.CreateTemp(b.TempDir(), "meta-")
			if err != nil {
				b.Fatal(err)
			}
			defer meta.Close()
			source := erofs.SourceEntries{BlockSize: 4096, ExternalDeviceBlocks: []uint64{16}}
			for i := 0; i < count; i++ {
				entry := erofs.SourceEntry{Path: fmt.Sprintf("/file-%04d", i), Mode: 0o444, Size: 16 * 4096}
				for block := int64(0); block < 16; block++ {
					entry.EncodedRanges = append(entry.EncodedRanges, erofs.EncodedRange{
						Device: 1, Offset: block * 4096, Size: 4096, EncodedSize: 4096, Algorithm: 254,
					})
				}
				source.Entries = append(source.Entries, entry)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := meta.Seek(0, 0); err != nil {
					b.Fatal(err)
				}
				writer := erofs.Create(meta, erofs.WithBlockSize(4096))
				if err := writer.CopyEntries(source, erofs.MetadataOnly()); err != nil {
					b.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestWriterEncodedRangeDeviceBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offset  int64
		encoded int64
		blocks  uint64
	}{
		{"next_device_start", 4096, 4096, 1},
		{"cross_device_end", 0, 8192, 1},
		{"offset_plus_size_overflow", math.MaxInt64 - 4095, 4096, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta, err := os.CreateTemp(t.TempDir(), "meta-")
			if err != nil {
				t.Fatal(err)
			}
			defer meta.Close()
			writer := erofs.Create(meta, erofs.WithBlockSize(4096))
			err = writer.CopyEntries(erofs.SourceEntries{
				BlockSize:            4096,
				ExternalDeviceBlocks: []uint64{tc.blocks, 2},
				Entries: []erofs.SourceEntry{{Path: "/file", Mode: 0o444, Size: 4096,
					EncodedRanges: []erofs.EncodedRange{{Device: 1, Offset: tc.offset, Size: 4096, EncodedSize: tc.encoded, Algorithm: 254}}}},
			}, erofs.MetadataOnly())
			if err == nil {
				err = writer.Close()
			}
			if !errors.Is(err, erofs.ErrInvalid) {
				t.Fatalf("out-of-device encoded range: got %v, want ErrInvalid", err)
			}
		})
	}
}
