// SPDX-License-Identifier: MIT
// Compression index interpretation follows erofs-utils lib/zmap.c
// (GPL-2.0+ OR MIT), Copyright 2018-2019 HUAWEI, Inc.
package erofs

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/erofs/go-erofs/internal/disk"
)

const (
	compressedNonHead    = 2
	compressedBlockCount = 1 << 11
	maxCompressedBytes   = 1 << 20
	maxDecodedBytes      = 64 << 20
)

type compressedIndex struct {
	kind, offset, back, ahead, blocks int64
	physical, next                    int64
	partial                           bool
}

type compressedMapping struct {
	logical, length, physical, encoded int64
	algorithm                          uint8
	inline, interlaced, partial        bool
}

type compressedFile struct {
	base       int64
	bits       uint8
	advise     uint16
	algorithms uint8
	inline     int64
	tail       int64
	packed     bool
	fragment   int64
}

func (b *file) compressionInfo(ino *inode) (*compressedFile, error) {
	base := (b.img.metaStartPos() + int64(ino.nid)*disk.SizeInodeCompact + ino.flatDataOffset() + 7) &^ 7
	var header [8]byte
	if n, err := b.img.meta.ReadAt(header[:], base); err != nil {
		return nil, err
	} else if n != len(header) {
		return nil, io.ErrUnexpectedEOF
	}
	c := &compressedFile{base: base, bits: b.img.sb.BlkSizeBits + (header[7] & 15), advise: binary.LittleEndian.Uint16(header[4:6]), algorithms: header[6], tail: -1}
	if header[7]&128 != 0 {
		c.packed = true
		c.fragment = int64(binary.LittleEndian.Uint64(header[:]) &^ (uint64(1) << 63))
		return c, nil
	}
	if c.bits > 20 || (ino.inodeLayout == disk.LayoutCompressedCompact && c.bits > 14) {
		return nil, fmt.Errorf("unsupported logical cluster size: %w", ErrNotImplemented)
	}
	if ino.inodeLayout == disk.LayoutCompressedFull && c.advise&1 != 0 {
		return nil, fmt.Errorf("compressed extent records: %w", ErrNotImplemented)
	}
	if c.advise&^uint16(63) != 0 {
		return nil, ErrNotImplemented
	}
	if ino.inodeLayout == disk.LayoutCompressedCompact && (c.advise&2 != 0) != (c.advise&4 != 0) {
		return nil, ErrInvalid
	}
	if c.advise&8 != 0 {
		c.inline = int64(binary.LittleEndian.Uint16(header[2:4]))
	}
	if c.advise&32 != 0 {
		c.fragment = int64(binary.LittleEndian.Uint32(header[:4]))
	}
	if c.inline != 0 || c.advise&32 != 0 {
		index, head, err := b.compressedHead(ino, c, ino.size-1)
		if err != nil {
			return nil, err
		}
		c.tail = head
		if c.inline != 0 {
			last, err := b.compressedIndex(ino, c, (ino.size-1)>>c.bits)
			if err != nil {
				return nil, err
			}
			c.fragment = last.next
		}
		if c.advise&32 != 0 && ino.inodeLayout == disk.LayoutCompressedFull {
			c.fragment |= index.physical << 32
		}
	}
	return c, nil
}

func (b *file) compressedIndex(ino *inode, c *compressedFile, lcn int64) (compressedIndex, error) {
	var result compressedIndex
	if lcn < 0 || lcn > (ino.size-1)>>c.bits {
		return result, ErrInvalid
	}
	if ino.inodeLayout == disk.LayoutCompressedFull {
		pos := c.base + 16 + lcn*8
		var data [8]byte
		if n, err := b.img.meta.ReadAt(data[:], pos); err != nil {
			return result, err
		} else if n != 8 {
			return result, io.ErrUnexpectedEOF
		}
		advise := binary.LittleEndian.Uint16(data[:2])
		result.kind = int64(advise & 3)
		result.next = pos + 8
		if result.kind == compressedNonHead {
			result.back = int64(binary.LittleEndian.Uint16(data[4:6]))
			result.ahead = int64(binary.LittleEndian.Uint16(data[6:]))
			if result.back&compressedBlockCount != 0 {
				if c.advise&6 == 0 {
					return result, ErrInvalid
				}
				result.blocks = result.back &^ compressedBlockCount
				result.back = 1
			}
		} else {
			result.offset = int64(binary.LittleEndian.Uint16(data[2:4]))
			result.physical = int64(binary.LittleEndian.Uint32(data[4:]))
			result.partial = advise&32768 != 0
		}
	} else {
		base := c.base + 8
		total := (ino.size-1)>>b.img.sb.BlkSizeBits + 1
		initial := ((32 - base%32) / 4) & 7
		middle := int64(0)
		if c.advise&1 != 0 && initial < total {
			middle = (total - initial) &^ 15
		}
		pos, shift := base, uint(2)
		relative := lcn
		if relative >= initial {
			pos += initial * 4
			relative -= initial
			if relative < middle {
				shift = 1
			} else {
				pos += middle * 2
				relative -= middle
			}
		}
		pos += relative << shift
		count := int64(2)
		if shift == 1 {
			if c.bits > 12 {
				return result, ErrNotImplemented
			}
			count = 16
		}
		packSize := count << shift
		packStart := pos &^ (packSize - 1)
		var pack [32]byte
		if n, err := b.img.meta.ReadAt(pack[:packSize], packStart); err != nil {
			return result, err
		} else if int64(n) != packSize {
			return result, io.ErrUnexpectedEOF
		}
		lowBits := max(c.bits, 12)
		width := uint((packSize - 4) * 8 / count)
		decode := func(i int64) (int64, int64) {
			bit := width * uint(i)
			v := binary.LittleEndian.Uint32(pack[bit/8:]) >> (bit & 7)
			return int64(v & ((1 << lowBits) - 1)), int64(v >> lowBits & 3)
		}
		i := (pos - packStart) >> shift
		lo, kind := decode(i)
		result.kind = kind
		result.next = packStart + packSize
		if kind == compressedNonHead {
			var last int64
			for j := i; j < count; j++ {
				value, typ := decode(j)
				last = value
				if typ != compressedNonHead {
					break
				}
				result.ahead++
			}
			if i+result.ahead == count && last&compressedBlockCount == 0 {
				result.ahead += last - 1
			}
			if lo&compressedBlockCount != 0 {
				if c.advise&2 == 0 {
					return result, ErrInvalid
				}
				result.blocks = lo &^ compressedBlockCount
				result.back = 1
			} else if i+1 != count {
				result.back = lo
			} else {
				lo, kind = decode(i - 1)
				if kind != compressedNonHead {
					lo = 0
				} else if lo&compressedBlockCount != 0 {
					lo = 1
				}
				result.back = lo + 1
			}
		} else {
			result.offset = lo
			blocks := int64(1)
			if c.advise&2 != 0 {
				blocks = 0
			}
			for j := i; j > 0; {
				j--
				value, typ := decode(j)
				if c.advise&2 == 0 {
					if typ == compressedNonHead {
						j -= value
					}
					if j >= 0 {
						blocks++
					}
					continue
				}
				if typ == compressedNonHead {
					if value&compressedBlockCount != 0 {
						j--
						blocks += value &^ compressedBlockCount
					} else {
						if value <= 1 {
							return result, ErrInvalid
						}
						j -= value - 2
					}
				} else {
					blocks++
				}
			}
			result.physical = int64(binary.LittleEndian.Uint32(pack[packSize-4:packSize])) + blocks
		}
	}
	if result.kind != compressedNonHead && result.offset >= 1<<c.bits {
		return result, ErrInvalid
	}
	return result, nil
}

func (b *file) compressedHead(ino *inode, c *compressedFile, off int64) (compressedIndex, int64, error) {
	lcn := off >> c.bits
	index, err := b.compressedIndex(ino, c, lcn)
	if err != nil {
		return index, 0, err
	}
	if index.kind != compressedNonHead && off&((1<<c.bits)-1) >= index.offset {
		return index, lcn, nil
	}
	back := index.back
	if index.kind != compressedNonHead {
		back = 1
	}
	for back > 0 && lcn >= back {
		lcn -= back
		index, err = b.compressedIndex(ino, c, lcn)
		if err != nil {
			return index, 0, err
		}
		if index.kind != compressedNonHead {
			return index, lcn, nil
		}
		back = index.back
	}
	return index, 0, fmt.Errorf("invalid compressed lookback: %w", ErrInvalid)
}

func (b *file) compressedMap(ino *inode, c *compressedFile, off int64) (compressedMapping, error) {
	var m compressedMapping
	if c.packed {
		return compressedMapping{logical: 0, length: ino.size, physical: c.fragment, algorithm: 255}, nil
	}
	head, lcn, err := b.compressedHead(ino, c, off)
	if err != nil {
		return m, err
	}
	m.logical = lcn<<c.bits | head.offset
	m.physical = head.physical << b.img.sb.BlkSizeBits
	m.partial = head.partial
	if c.advise&32 != 0 && lcn == c.tail {
		m.algorithm = 255
		m.physical = c.fragment
	} else if c.inline != 0 && lcn == c.tail {
		m.inline = true
		m.physical = c.fragment
		m.encoded = c.inline
	} else {
		blocks := int64(1)
		big := c.advise&2 != 0
		if head.kind == 0 || head.kind == 3 {
			big = c.advise&4 != 0
		}
		if big && (lcn+1)<<c.bits < ino.size {
			next, e := b.compressedIndex(ino, c, lcn+1)
			if e != nil {
				return m, e
			}
			if next.kind == compressedNonHead {
				if next.back != 1 || next.blocks == 0 {
					return m, ErrInvalid
				}
				blocks = next.blocks
			}
		}
		m.encoded = blocks << b.img.sb.BlkSizeBits
	}
	if m.algorithm != 255 {
		if head.kind == 0 {
			m.algorithm = 254
			m.interlaced = c.advise&16 != 0
		} else if head.kind == 3 {
			m.algorithm = c.algorithms >> 4
		} else {
			m.algorithm = c.algorithms & 15
		}
	}
	end := ino.size
	for next := lcn + 1; next <= (ino.size-1)>>c.bits; {
		index, e := b.compressedIndex(ino, c, next)
		if e != nil {
			return m, e
		}
		if index.kind != compressedNonHead {
			end = next<<c.bits | index.offset
			break
		}
		next += max(index.ahead, 1)
	}
	m.length = min(end, ino.size) - m.logical
	if m.logical < 0 || m.length <= 0 || off < m.logical || off >= m.logical+m.length || m.encoded > maxCompressedBytes || m.length > maxDecodedBytes {
		return m, fmt.Errorf("invalid compressed extent: %w", ErrInvalid)
	}
	if m.inline && m.physical%(1<<b.img.sb.BlkSizeBits)+m.encoded > 1<<b.img.sb.BlkSizeBits {
		return m, ErrInvalid
	}
	return m, nil
}

func decodeLZ4(src, dst []byte, partial bool) (int, error) {
	in, out := 0, 0
	length := func(initial int) (int, error) {
		n := initial
		if initial == 15 {
			for {
				if in >= len(src) {
					return 0, io.ErrUnexpectedEOF
				}
				v := int(src[in])
				in++
				if n > maxDecodedBytes-v {
					return 0, ErrInvalid
				}
				n += v
				if v != 255 {
					break
				}
			}
		}
		return n, nil
	}
	for in < len(src) && out < len(dst) {
		token := src[in]
		in++
		n, err := length(int(token >> 4))
		if err != nil {
			return out, err
		}
		if n > len(src)-in {
			return out, io.ErrUnexpectedEOF
		}
		if !partial && n > len(dst)-out {
			return out, ErrInvalid
		}
		copied := min(n, len(dst)-out)
		copy(dst[out:], src[in:in+copied])
		in += n
		out += copied
		if out == len(dst) {
			if !partial && in != len(src) {
				return out, ErrInvalid
			}
			return out, nil
		}
		if in+2 > len(src) {
			return out, io.ErrUnexpectedEOF
		}
		distance := int(binary.LittleEndian.Uint16(src[in:]))
		in += 2
		if distance == 0 || distance > out {
			return out, ErrInvalid
		}
		n, err = length(int(token & 15))
		if err != nil {
			return out, err
		}
		n += 4
		if !partial && n >= len(dst)-out {
			return out, ErrInvalid
		}
		for range min(n, len(dst)-out) {
			dst[out] = dst[out-distance]
			out++
		}
	}
	if out != len(dst) {
		return out, io.ErrUnexpectedEOF
	}
	return out, nil
}

func (b *file) compressedReadAt(p []byte, off int64) (int, error) {
	if b.img.metadataOnly {
		return 0, ErrNotImplemented
	}
	ino := b.info
	c := b.compressed
	var total int
	for len(p) > 0 {
		if off >= ino.size {
			return total, io.EOF
		}
		m, err := b.compressedMap(ino, c, off)
		if err != nil {
			return total, err
		}
		if m.algorithm == 255 {
			if b.img.sb.PackedNid == ino.nid {
				return total, ErrInvalid
			}
			length := min(int64(len(p)), m.logical+m.length-off)
			if m.physical > int64(^uint64(0)>>1)-(off-m.logical) {
				return total, ErrInvalid
			}
			packed := &file{img: b.img, nid: b.img.sb.PackedNid}
			n, err := packed.ReadAt(p[:length], m.physical+off-m.logical)
			_ = packed.Close()
			total += n
			if err != nil {
				return total, err
			}
			off += int64(n)
			p = p[n:]
			continue
		}
		decoded := make([]byte, m.length)
		{
			encoded := make([]byte, m.encoded)
			device, at, e := b.img.mapDev(0, m.physical)
			if e != nil {
				return total, e
			}
			if m.inline {
				device, at = b.img.meta, m.physical
			}
			n, e := device.ReadAt(encoded, at)
			if e != nil || n != len(encoded) {
				if e == nil {
					e = io.ErrUnexpectedEOF
				}
				return total, e
			}
			switch m.algorithm {
			case 254:
				if m.length > int64(len(encoded)) {
					return total, ErrInvalid
				}
				if m.interlaced {
					if m.encoded > 1<<b.img.sb.BlkSizeBits {
						return total, ErrInvalid
					}
					pivot := int(m.logical % (1 << b.img.sb.BlkSizeBits))
					n := copy(decoded, encoded[pivot:])
					copy(decoded[n:], encoded[:pivot])
				} else {
					copy(decoded, encoded)
				}
			case 0:
				if b.img.sb.FeatureIncompat&disk.FeatureIncompatLZ4_0Padding != 0 {
					for len(encoded) > 0 && encoded[0] == 0 {
						encoded = encoded[1:]
					}
				}
				_, err = decodeLZ4(encoded, decoded, m.partial || b.img.sb.FeatureIncompat&disk.FeatureIncompatLZ4_0Padding == 0)
			case 2:
				for len(encoded) > 0 && encoded[0] == 0 {
					encoded = encoded[1:]
				}
				reader := flate.NewReader(bytes.NewReader(encoded))
				_, err = io.ReadFull(reader, decoded)
				if err == nil && !m.partial {
					var extra [1]byte
					n, endErr := reader.Read(extra[:])
					if n != 0 || endErr != io.EOF {
						if endErr != nil {
							err = endErr
						} else {
							err = ErrInvalid
						}
					}
				}
				_ = reader.Close()
			default:
				return total, fmt.Errorf("compression algorithm %d: %w", m.algorithm, ErrNotImplemented)
			}
		}
		if err != nil {
			return total, err
		}
		n := copy(p, decoded[off-m.logical:])
		total += n
		off += int64(n)
		p = p[n:]
	}
	return total, nil
}
