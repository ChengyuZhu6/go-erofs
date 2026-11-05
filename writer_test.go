package erofs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"testing"

	"github.com/erofs/go-erofs/internal/builder"
	"github.com/erofs/go-erofs/internal/disk"
)

func TestWriterDeviceTableRoundTrip(t *testing.T) {
	meta, err := os.CreateTemp(t.TempDir(), "meta-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = meta.Close() }()
	writer := Create(meta, WithBlockSize(4096))
	if err := writer.CopyEntries(SourceEntries{
		BlockSize:            4096,
		ExternalDeviceBlocks: []uint64{2, 3},
		Entries: []SourceEntry{{Path: "/file", Mode: 0o444, Size: 4096,
			DataRanges: []DataRange{{Device: 1, Offset: 4096, Size: 4096}}}},
	}, MetadataOnly()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	devices := []io.ReaderAt{bytes.NewReader(make([]byte, 8192)), bytes.NewReader(make([]byte, 12288))}
	for generation := 1; generation <= 2; generation++ {
		raw, err := os.ReadFile(meta.Name())
		if err != nil {
			t.Fatal(err)
		}
		offset := int(binary.LittleEndian.Uint16(raw[1112:1114])) * 128
		if offset != 1152 {
			t.Fatalf("generation %d: device table offset = %d, want 1152", generation, offset)
		}
		for i, blocks := range []uint32{2, 3} {
			if got := binary.LittleEndian.Uint32(raw[offset+i*128+64 : offset+i*128+68]); got != blocks {
				t.Fatalf("generation %d device %d: blocks=%d, want %d", generation, i+1, got, blocks)
			}
		}
		parent, err := Open(bytes.NewReader(raw), WithExtraDevices(devices...))
		if err != nil {
			t.Fatal(err)
		}
		next, err := os.CreateTemp(t.TempDir(), "meta-")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = next.Close() }()
		child := Create(next, WithBlockSize(4096))
		if err := child.CopyFrom(parent, MetadataOnly()); err != nil {
			t.Fatal(err)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
		meta = next
	}
}

func TestLoadFlatMappedDevice(t *testing.T) {
	payload := append(bytes.Repeat([]byte("a"), 4096), bytes.Repeat([]byte("b"), 4096)...)
	for _, tc := range []struct {
		name   string
		layout uint8
		pos    int64
	}{
		{"plain_first", disk.LayoutFlatPlain, 0},
		{"plain_second", disk.LayoutFlatPlain, 4096},
		{"inline_head", disk.LayoutFlatInline, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := image{
				sb:      disk.SuperBlock{BlkSizeBits: 12},
				meta:    bytes.NewReader(make([]byte, 8192)),
				devices: []deviceInfo{{device: bytes.NewReader(payload), mappedBlkAddr: 2, blocks: 2}},
			}
			img.blkPool.New = func() any { return &block{buf: make([]byte, 4096)} }
			ino := inode{inodeLayout: tc.layout, inodeData: 2, size: 8192}
			blk, err := img.loadBlock(&ino, tc.pos)
			if err != nil {
				t.Fatal(err)
			}
			defer img.putBlock(blk)
			if !bytes.Equal(blk.bytes(), payload[tc.pos:tc.pos+4096]) {
				t.Fatal("flat extent read metadata instead of the mapped external device")
			}
		})
	}
}

func TestChunkBitsFromChunks(t *testing.T) {
	const blockSize = 4096
	hole := builder.Chunk{PhysicalBlock: builder.NullPhysicalBlock}

	data := func(phys uint64, count uint16) builder.Chunk {
		return builder.Chunk{PhysicalBlock: phys, Count: count, DeviceID: 1}
	}
	h := func(count uint16) builder.Chunk {
		c := hole
		c.Count = count
		return c
	}

	tests := []struct {
		name   string
		chunks []builder.Chunk
		size   uint64
		want   uint8
	}{
		{
			name: "empty chunks is one hole",
			size: 80 << 30,
			want: minChunkBits(80<<30, blockSize, 0),
		},
		{
			name:   "one data range",
			chunks: []builder.Chunk{data(10, 2)},
			size:   2 * blockSize,
			want:   minChunkBits(2*blockSize, blockSize, 0),
		},
		{
			name:   "uint16 split still one run",
			chunks: []builder.Chunk{data(0, 65535), data(65535, 1)},
			size:   65536 * blockSize,
			want:   minChunkBits(65536*blockSize, blockSize, 0),
		},
		{
			name:   "64KiB data-hole-data",
			chunks: []builder.Chunk{data(0, 16), h(16), data(100, 16)},
			size:   48 * blockSize,
			want:   4,
		},
		{
			name:   "disjoint 4KiB ranges stay 4KiB",
			chunks: []builder.Chunk{data(5, 1), data(20, 3)},
			size:   4 * blockSize,
			want:   0,
		},
		{
			name:   "4KiB hole pins 4KiB chunks",
			chunks: []builder.Chunk{data(0, 16), h(1), data(16, 15)},
			size:   32 * blockSize,
			want:   0,
		},
		{
			name:   "whole-file hole",
			chunks: []builder.Chunk{h(16)},
			size:   16 * blockSize,
			want:   minChunkBits(16*blockSize, blockSize, 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chunkBitsFromChunks(tt.chunks, tt.size, blockSize, 0)
			if got != tt.want {
				t.Fatalf("chunkBitsFromChunks = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFileReadAt(t *testing.T) {
	first := bytes.Repeat([]byte{0x31}, 4096)
	second := bytes.Repeat([]byte{0x72}, 4096)
	for _, size := range []int{0, 37, 4096, 8193, 4*4096 + 512} {
		want := make([]byte, size)
		for i := range want {
			want[i] = byte(i*17 + i/4096)
		}
		for _, external := range []bool{false, true} {
			meta, err := os.CreateTemp(t.TempDir(), "meta-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = meta.Close() }()
			entry := SourceEntry{Path: "/file", Mode: 0o444, Size: int64(size), Data: bytes.NewReader(want)}
			source := SourceEntries{BlockSize: 4096, Entries: []SourceEntry{entry}}
			var options []CopyOpt
			if external {
				entry.Data = nil
				want = make([]byte, size)
				for at := 0; at < size; at += 4096 {
					n := min(4096, size-at)
					r := DataRange{Offset: -1, Size: int64(n)}
					if at/4096%3 != 1 {
						r.Device, r.Offset = uint16(at/4096%2), 0
						payload := first
						if r.Device == 1 {
							payload = second
						}
						copy(want[at:at+n], payload[:n])
					}
					entry.DataRanges = append(entry.DataRanges, r)
				}
				source.Entries[0] = entry
				source.ExternalDeviceBlocks = []uint64{1, 1}
				options = []CopyOpt{MetadataOnly()}
			}
			writer := Create(meta, WithBlockSize(4096))
			if err := writer.CopyEntries(source, options...); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			var readOptions []OpenOpt
			if external {
				readOptions = []OpenOpt{WithExtraDevices(bytes.NewReader(first), bytes.NewReader(second))}
			}
			image, err := Open(meta, readOptions...)
			if err != nil {
				t.Fatal(err)
			}
			opened, err := image.Open("file")
			if err != nil {
				t.Fatal(err)
			}
			f := opened.(*file)
			stat, err := meta.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := f.ValidateData([]int64{stat.Size(), 4096, 4096}); err != nil {
				t.Fatalf("size=%d external=%t validation: %v", size, external, err)
			}
			if external && size > 0 && f.ValidateData([]int64{stat.Size(), 0, 0}) == nil {
				t.Fatal("device byte limit ignored")
			}
			var wg sync.WaitGroup
			for _, at := range []int64{0, 31, 4090, int64(size), int64(size + 1)} {
				at := at
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 20 {
						p := bytes.Repeat([]byte{0xff}, 128)
						n, err := f.ReadAt(p, at)
						expected := min(len(p), max(0, size-int(at)))
						if n != expected || (expected < len(p) && !errors.Is(err, io.EOF)) || (expected == len(p) && err != nil) {
							t.Errorf("size=%d external=%t at=%d n=%d err=%v", size, external, at, n, err)
						} else if expected > 0 && !bytes.Equal(p[:n], want[at:at+int64(n)]) {
							t.Errorf("size=%d external=%t read data differs at %d layout=%d", size, external, at, f.info.inodeLayout)
						}
						if _, err := f.Stat(); err != nil {
							t.Error(err)
						}
					}
				}()
			}
			wg.Wait()
			if n, err := f.ReadAt(nil, int64(size+1)); n != 0 || err != nil {
				t.Fatalf("empty read: %d %v", n, err)
			}
			if _, err := f.ReadAt(nil, -1); !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("negative offset: %v", err)
			}
			p := make([]byte, size)
			if _, err := io.ReadFull(f, p); err != nil || !bytes.Equal(p, want) {
				t.Fatalf("ReadAt changed cursor: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, fs.ErrClosed) {
				t.Fatalf("read after close: %v", err)
			}
		}
	}
}

func TestFileReadAtMappedDeviceBoundary(t *testing.T) {
	meta := bytes.Repeat([]byte{0x31}, 4*4096)
	extra := bytes.Repeat([]byte{0x72}, 4096)
	for _, layout := range []uint8{disk.LayoutFlatPlain, disk.LayoutFlatInline} {
		f := &file{img: &image{
			sb: disk.SuperBlock{BlkSizeBits: 12}, meta: bytes.NewReader(meta),
			devices: []deviceInfo{{device: bytes.NewReader(extra), mappedBlkAddr: 2, blocks: 1}},
		}, info: &inode{mode: 0o444, size: 2*4096 + 37, inodeData: 1, icsize: 32, inodeLayout: layout}}
		if err := f.ValidateData([]int64{int64(len(meta)), 4096}); err != nil {
			t.Fatal(err)
		}
		if f.ValidateData([]int64{int64(len(meta)), 0}) == nil {
			t.Fatal("mapped device length was not checked")
		}
		whole := make([]byte, f.info.size)
		if n, err := f.ReadAt(whole, 0); n != len(whole) || err != nil {
			t.Fatalf("whole read: n=%d err=%v", n, err)
		}
		for off := 0; off < len(whole); off += 4096 {
			part := make([]byte, min(4096, len(whole)-off))
			if _, err := f.ReadAt(part, int64(off)); err != nil || !bytes.Equal(part, whole[off:off+len(part)]) {
				t.Fatalf("request size changes mapped data at %d: %v", off, err)
			}
		}
		if !bytes.Equal(whole[4096:8192], extra) || !bytes.Equal(whole[:4096], meta[:4096]) {
			t.Fatal("mapped device contents differ")
		}
	}
}

func TestCompressedIndexLookbackBoundaries(t *testing.T) {
	w := &erofsWriter{blockSize: 512}
	for _, count := range []int64{2047, 2048, 6146} {
		e := &erofsEntry{size: uint64(count * 512), layout: disk.LayoutCompressedFull, encodedRanges: []EncodedRange{{Size: count * 512, EncodedSize: 512}}}
		var table bytes.Buffer
		if err := w.writeCompressedIndexes(&table, e); err != nil {
			t.Fatal(err)
		}
		for i := int64(2); i < count; i++ {
			back := binary.LittleEndian.Uint16(table.Bytes()[20+i*8:])
			if back != uint16(min(i, compressedBlockCount-1)) {
				t.Fatalf("lookback at %d = %d", i, back)
			}
		}
		f := &file{img: &image{meta: bytes.NewReader(table.Bytes()), sb: disk.SuperBlock{BlkSizeBits: 9}}}
		ino := &inode{size: count * 512, inodeLayout: disk.LayoutCompressedFull}
		c := &compressedFile{bits: 9, advise: 6}
		if _, head, err := f.compressedHead(ino, c, ino.size-1); err != nil || head != 0 {
			t.Fatalf("long lookback count=%d head=%d err=%v", count, head, err)
		}
	}
	for _, blocks := range []int64{compressedBlockCount - 1, compressedBlockCount} {
		e := &erofsEntry{size: 1024, layout: disk.LayoutCompressedFull, encodedRanges: []EncodedRange{{Size: 1024, EncodedSize: blocks * 512}}}
		var table bytes.Buffer
		err := w.writeCompressedIndexes(&table, e)
		if (blocks < compressedBlockCount) != (err == nil) {
			t.Fatalf("compressed block count=%d err=%v", blocks, err)
		}
	}
}

func TestLZ4CodecAndMalformedBlocks(t *testing.T) {
	for _, size := range []int{0, 5, 12, 255, 4096, 65536} {
		data := bytes.Repeat([]byte("abcde"), (size+4)/5)[:size]
		encoded := compressLZ4(data)
		decoded := make([]byte, size)
		if n, err := decodeLZ4(encoded, decoded, false); err != nil || n != size || !bytes.Equal(decoded, data) {
			t.Fatalf("LZ4 roundtrip size=%d n=%d err=%v", size, n, err)
		}
		if size > 0 {
			prefix := make([]byte, min(size, 11))
			if n, err := decodeLZ4(encoded, prefix, true); err != nil || n != len(prefix) || !bytes.Equal(prefix, data[:len(prefix)]) {
				t.Fatalf("LZ4 prefix size=%d n=%d err=%v", size, n, err)
			}
			if _, err := decodeLZ4(append(encoded, 0), decoded, false); err == nil {
				t.Fatal("complete LZ4 accepted trailing input")
			}
			if _, err := decodeLZ4(encoded[:len(encoded)-1], decoded, false); err == nil {
				t.Fatal("complete LZ4 accepted truncated input")
			}
		}
	}
	for _, encoded := range [][]byte{{0x10, 'a', 0, 0}, {0x10, 'a', 2, 0}, {0xf0, 255}} {
		if _, err := decodeLZ4(encoded, make([]byte, 16), false); err == nil {
			t.Fatalf("malformed LZ4 accepted: %x", encoded)
		}
	}
}

func TestFileReadAtRejectsInvalidMapping(t *testing.T) {
	for _, format := range []uint16{0, disk.LayoutChunkFormatIndexes, disk.LayoutChunkFormat48Bit, 0x8000} {
		for _, truncated := range []bool{false, true} {
			index := make([]byte, 64)
			entry := index[32:40]
			if format&disk.LayoutChunkFormatIndexes != 0 {
				binary.LittleEndian.PutUint32(entry[4:], 1)
			} else {
				binary.LittleEndian.PutUint32(entry, 1)
			}
			if truncated {
				index = index[:33]
			}
			f := &file{img: &image{sb: disk.SuperBlock{BlkSizeBits: 12}, meta: bytes.NewReader(index)}, info: &inode{mode: 0o444, size: 4096, icsize: 32, inodeLayout: disk.LayoutChunkBased, inodeData: uint32(format)}}
			err := f.ValidateData([]int64{8192})
			if truncated || format == disk.LayoutChunkFormat48Bit || format == 0x8000 {
				if err == nil {
					t.Fatalf("invalid mapping accepted: format=%x truncated=%t", format, truncated)
				}
			} else if err != nil {
				t.Fatalf("valid chunk format=%x rejected: %v", format, err)
			}
		}
	}
}
