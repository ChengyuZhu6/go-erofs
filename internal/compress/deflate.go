package compress

import (
	"bytes"
	"compress/flate"
	"io"
)

// decompressDeflate 使用DEFLATE算法解压缩数据
func decompressDeflate(req *DecompressRequest) error {
	// 处理输入数据的0填充
	inputMargin := FixupInputSize(req.In)
	if inputMargin >= req.InputSize {
		return ErrCorruptedData
	}

	// 创建DEFLATE读取器
	flateReader := flate.NewReader(bytes.NewReader(req.In[inputMargin:req.InputSize]))
	defer flateReader.Close()

	var dest []byte
	var tempBuf []byte

	// 如果需要跳过部分数据，分配临时缓冲区
	if req.DecodedSkip > 0 {
		tempBuf = make([]byte, req.DecodedLength)
		dest = tempBuf
	} else {
		dest = req.Out
	}

	// 执行DEFLATE解压缩
	n, err := io.ReadFull(flateReader, dest)
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
