package erofs

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
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
