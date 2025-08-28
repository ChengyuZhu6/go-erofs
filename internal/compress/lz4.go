package compress

import (
	"github.com/pierrec/lz4/v4"
)

// decompressLZ4 使用LZ4算法解压缩数据
func decompressLZ4(req *DecompressRequest) error {
	// 处理输入数据的0填充
	inputMargin := FixupInputSize(req.In)
	if inputMargin >= req.InputSize {
		return ErrCorruptedData
	}

	var dest []byte
	var tempBuf []byte

	// 如果需要跳过部分数据，分配临时缓冲区
	if req.DecodedSkip > 0 {
		tempBuf = make([]byte, req.DecodedLength)
		dest = tempBuf
	} else {
		dest = req.Out
	}

	// 执行LZ4解压缩
	n, err := lz4.UncompressBlock(req.In[inputMargin:req.InputSize], dest)
	if err != nil {
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
