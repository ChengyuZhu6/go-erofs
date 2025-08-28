package compress

import (
	"bytes"
	"io"

	"github.com/klauspost/compress/zstd"
)

// decompressZSTD 使用ZSTD算法解压缩数据
func decompressZSTD(req *DecompressRequest) error {
	// 处理输入数据的0填充
	inputMargin := FixupInputSize(req.In)
	if inputMargin >= req.InputSize {
		return ErrCorruptedData
	}

	// 创建ZSTD解压缩读取器
	zstdReader, err := zstd.NewReader(bytes.NewReader(req.In[inputMargin:req.InputSize]))
	if err != nil {
		return err
	}
	defer zstdReader.Close()

	var dest []byte
	var tempBuf []byte

	// 如果需要跳过部分数据，分配临时缓冲区
	if req.DecodedSkip > 0 {
		tempBuf = make([]byte, req.DecodedLength)
		dest = tempBuf
	} else {
		dest = req.Out
	}

	// 执行ZSTD解压缩
	n, err := io.ReadFull(zstdReader, dest)
	if err != nil && err != io.ErrUnexpectedEOF {
		return err
	}

	if uint32(n) != req.DecodedLength {
		return ErrCorruptedData
	}

	// 如果需要，复制跳过部分后的数据到输出缓冲区
	if req.DecodedSkip > 0 {
		copy(req.Out, dest[req.DecodedSkip:req.DecodedLength])
	}

	return nil
}
