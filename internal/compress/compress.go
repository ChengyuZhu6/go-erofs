// Package compress 提供EROFS文件系统的压缩和解压缩功能
package compress

import (
	"errors"
	"fmt"
)

// 定义支持的压缩算法常量
const (
	AlgorithmNone    = 0
	AlgorithmLZ4     = 1
	AlgorithmZSTD    = 2
	AlgorithmLZMA    = 3
	AlgorithmDeflate = 4
)

// DecompressRequest 包含解压缩请求的参数
type DecompressRequest struct {
	// 输入和输出缓冲区
	In  []byte
	Out []byte

	// 解压缩参数
	DecodedSkip      uint32
	InputSize        uint32
	DecodedLength    uint32
	InterlacedOffset uint32
	Algorithm        uint32

	// 是否部分解码
	PartialDecoding bool
}

// 定义错误类型
var (
	// ErrUnsupportedCompression 表示不支持的压缩算法
	ErrUnsupportedCompression = errors.New("unsupported compression algorithm")

	// ErrNotImplemented 表示功能尚未实现
	ErrNotImplemented = errors.New("compression feature not implemented")

	// ErrCorruptedData 表示压缩数据已损坏
	ErrCorruptedData = errors.New("corrupted compressed data")
)

// Decompress 根据指定的算法解压缩数据
func Decompress(req *DecompressRequest) error {
	switch req.Algorithm {
	case AlgorithmLZ4:
		return decompressLZ4(req)
	case AlgorithmZSTD:
		return decompressZSTD(req)
	case AlgorithmDeflate:
		return decompressDeflate(req)
	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedCompression, req.Algorithm)
	}
}

// FixupInputSize 修复输入大小，处理0填充
func FixupInputSize(data []byte) uint32 {
	for i := 0; i < len(data); i++ {
		if data[i] != 0 {
			return uint32(i)
		}
	}
	return uint32(len(data))
}
