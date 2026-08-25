package erofs

import (
	"testing"

	"github.com/erofs/go-erofs/internal/builder"
)

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
