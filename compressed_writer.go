package erofs

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/erofs/go-erofs/internal/disk"
)

// EncodedRange describes compressed bytes, never an uncompressed DataRange.
// Device zero addresses metadata; nonzero devices follow the image device table.
// Size is decoded length. EncodedSize is block-aligned stored length.
// Partial permits the stored stream to decode beyond Size.
type EncodedRange struct {
	Device                    uint16
	Offset, Size, EncodedSize int64
	Algorithm                 uint8
	Partial                   bool
}

func WithCompression() CreateOpt {
	return func(o *createOptions) { o.compress = true }
}

func WithHotFiles(paths ...string) CreateOpt {
	return func(o *createOptions) { o.hotFiles = append(o.hotFiles, paths...) }
}

func (w *Writer) HotDataEnd() int64 { return w.hotDataEnd }

func (fi *fileInfo) EncodedRanges() ([]EncodedRange, error) {
	if fi.encodedLoader == nil {
		return nil, nil
	}
	return fi.encodedLoader()
}

func (b *file) encodedRanges(ino *inode) ([]EncodedRange, error) {
	c, err := b.compressionInfo(ino)
	if err != nil {
		return nil, err
	}
	var ranges []EncodedRange
	for off := int64(0); off < ino.size; {
		m, err := b.compressedMap(ino, c, off)
		if err != nil {
			return nil, err
		}
		if m.inline || m.algorithm == 255 || m.interlaced {
			return nil, ErrNotImplemented
		}
		r := EncodedRange{Offset: m.physical, Size: m.length, EncodedSize: m.encoded, Algorithm: m.algorithm, Partial: m.partial}
		for i, d := range b.img.devices {
			start := int64(d.mappedBlkAddr) << b.img.sb.BlkSizeBits
			end := start + int64(d.blocks)<<b.img.sb.BlkSizeBits
			if d.mappedBlkAddr != 0 && r.Offset >= start && r.Offset < end {
				if r.EncodedSize > end-r.Offset {
					return nil, ErrInvalid
				}
				r.Device = uint16(i + 1)
				r.Offset -= start
				break
			}
		}
		ranges = append(ranges, r)
		off += r.Size
	}
	return ranges, nil
}

func compressLZ4(src []byte) []byte {
	var table [1 << 16]int
	out := make([]byte, 0, len(src))
	anchor, at := 0, 0
	extend := func(n int) {
		for n >= 255 {
			out = append(out, 255)
			n -= 255
		}
		out = append(out, byte(n))
	}
	for at+12 <= len(src) {
		value := binary.LittleEndian.Uint32(src[at:])
		hash := (value * 2654435761) >> 16
		match := table[hash] - 1
		table[hash] = at + 1
		if match < 0 || at-match > 65535 || binary.LittleEndian.Uint32(src[match:]) != value {
			at++
			continue
		}
		length := 4
		for at+length < len(src)-5 && src[match+length] == src[at+length] {
			length++
		}
		literal := at - anchor
		out = append(out, byte(min(literal, 15)<<4|min(length-4, 15)))
		if literal >= 15 {
			extend(literal - 15)
		}
		out = append(out, src[anchor:at]...)
		distance := at - match
		out = append(out, byte(distance), byte(distance>>8))
		if length-4 >= 15 {
			extend(length - 4 - 15)
		}
		at += length
		anchor = at
	}
	literal := len(src) - anchor
	out = append(out, byte(min(literal, 15)<<4))
	if literal >= 15 {
		extend(literal - 15)
	}
	return append(out, src[anchor:]...)
}

func (w *Writer) compressTree(root *erofsEntry) (*os.File, error) {
	var spool *os.File
	var visit func(*erofsEntry) error
	visit = func(e *erofsEntry) error {
		if e.hardlink != nil {
			return nil
		}
		if e.mode&disk.StatTypeMask == disk.StatTypeReg && e.size > 0 && e.data != nil && len(e.encodedRanges) == 0 {
			if w.dataFile == nil && spool == nil {
				var err error
				spool, err = os.CreateTemp(w.tempDir, "erofs-compressed-*")
				if err != nil {
					return err
				}
			}
			output := spool
			device := uint16(0)
			if w.dataFile != nil {
				if err := w.ensureDataDevice(); err != nil {
					return err
				}
				output = w.dataFile
				device = w.dataDeviceID
			}
			start, err := output.Seek(0, io.SeekEnd)
			if err != nil {
				return err
			}
			if start%int64(w.blockSize) != 0 {
				return fmt.Errorf("unaligned compression output: %w", ErrInvalid)
			}
			input := e.data
			buffer := make([]byte, 16*w.blockSize)
			for remaining := int64(e.size); remaining > 0; {
				length := min(int64(len(buffer)), remaining)
				if _, err := io.ReadFull(input, buffer[:length]); err != nil {
					return err
				}
				compressed := compressLZ4(buffer[:length])
				if len(compressed)+w.blockSize-1 >= int(length) {
					for at := int64(0); at < length; at += int64(w.blockSize) {
						n := min(int64(w.blockSize), length-at)
						offset, err := output.Seek(0, io.SeekCurrent)
						if err != nil {
							return err
						}
						block := make([]byte, w.blockSize)
						copy(block, buffer[at:at+n])
						if n, err := output.Write(block); err != nil {
							return err
						} else if n != len(block) {
							return io.ErrShortWrite
						}
						e.encodedRanges = append(e.encodedRanges, EncodedRange{Device: device, Offset: offset, Size: n, EncodedSize: int64(w.blockSize), Algorithm: 254})
					}
				} else {
					offset, err := output.Seek(0, io.SeekCurrent)
					if err != nil {
						return err
					}
					stored := (len(compressed) + w.blockSize - 1) / w.blockSize * w.blockSize
					block := make([]byte, stored)
					copy(block[stored-len(compressed):], compressed)
					if n, err := output.Write(block); err != nil {
						return err
					} else if n != len(block) {
						return io.ErrShortWrite
					}
					e.encodedRanges = append(e.encodedRanges, EncodedRange{Device: device, Offset: offset, Size: length, EncodedSize: int64(stored)})
				}
				remaining -= length
			}
			if closer, ok := input.(io.Closer); ok {
				_ = closer.Close()
			}
			end, err := output.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}
			e.encodedSize = end - start
			if w.dataFile != nil {
				w.dataOff = end
				e.data = nil
			} else {
				e.data = io.NewSectionReader(spool, start, end-start)
				for i := range e.encodedRanges {
					e.encodedRanges[i].Offset -= start
				}
			}
		}
		return nil
	}
	queue := []*erofsEntry{root}
	for i := 0; i < len(queue); i++ {
		queue = append(queue, queue[i].children...)
	}
	hot := map[string]bool{}
	for _, name := range w.hotFiles {
		hot[cleanPath(name)] = true
	}
	for _, e := range queue {
		if hot[e.path] && e.hardlink != nil {
			hot[e.hardlink.path] = true
		}
	}
	sort.SliceStable(queue, func(i, j int) bool { return hot[queue[i].path] && !hot[queue[j].path] })
	var err error
	for _, e := range queue {
		if err = visit(e); err != nil {
			break
		}
		if hot[e.path] {
			w.hotDataEnd = w.dataOff
		}
	}
	if err != nil && spool != nil {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
		spool = nil
	}
	return spool, err
}

func (w *erofsWriter) writeCompressedIndexes(out io.Writer, e *erofsEntry) error {
	count := (int64(e.size)-1)/int64(w.blockSize) + 1
	if count > (maxChunkIndexBytes-16)/8 {
		return ErrInvalid
	}
	indexes := make([]byte, 16+count*8)
	binary.LittleEndian.PutUint16(indexes[4:6], 6)
	var logical int64
	for _, r := range e.encodedRanges {
		if r.Size <= 0 || r.Size > int64(e.size)-logical || r.Offset < 0 || r.Offset%int64(w.blockSize) != 0 || r.EncodedSize <= 0 || r.EncodedSize%int64(w.blockSize) != 0 || r.EncodedSize > maxCompressedBytes || int(r.Device) > len(w.devices) {
			return ErrInvalid
		}
		if logical+r.Size < int64(e.size) && logical/int64(w.blockSize) == (logical+r.Size)/int64(w.blockSize) {
			return fmt.Errorf("multiple compressed heads in one cluster: %w", ErrNotImplemented)
		}
		physical := uint64(r.Offset / int64(w.blockSize))
		if r.Device != 0 {
			physical += uint64(w.sbAreaBlocks() + (w.metadataBytes()+w.blockSize-1)/w.blockSize)
			for _, entry := range w.entries {
				physical += uint64((w.flatPlainDataSize(entry) + w.blockSize - 1) / w.blockSize)
			}
			for _, blocks := range w.devices[:r.Device-1] {
				physical += blocks
			}
		} else if e.data != nil {
			physical += uint64(e.dataBlkAddr)
		}
		if physical > 1<<32-1 {
			return ErrInvalid
		}
		first := logical / int64(w.blockSize)
		kind := uint16(1)
		if r.Algorithm == 254 {
			kind = 0
			if r.Size > r.EncodedSize {
				return ErrInvalid
			}
		} else if r.Algorithm != 0 {
			return ErrNotImplemented
		}
		if r.Partial {
			kind |= 32768
		}
		binary.LittleEndian.PutUint16(indexes[16+first*8:], kind)
		binary.LittleEndian.PutUint16(indexes[18+first*8:], uint16(logical%int64(w.blockSize)))
		binary.LittleEndian.PutUint32(indexes[20+first*8:], uint32(physical))
		last := (logical + r.Size - 1) / int64(w.blockSize)
		if logical+r.Size < int64(e.size) {
			last = (logical+r.Size)/int64(w.blockSize) - 1
		}
		for index := first + 1; index <= last; index++ {
			binary.LittleEndian.PutUint16(indexes[16+index*8:], 2)
			back := index - first
			if index == first+1 {
				back = compressedBlockCount + r.EncodedSize/int64(w.blockSize)
			}
			if back > 65535 || last-index+1 > 65535 {
				return ErrInvalid
			}
			binary.LittleEndian.PutUint16(indexes[20+index*8:], uint16(back))
			binary.LittleEndian.PutUint16(indexes[22+index*8:], uint16(last-index+1))
		}
		logical += r.Size
	}
	if logical != int64(e.size) {
		return ErrInvalid
	}
	_, err := out.Write(indexes)
	return err
}
