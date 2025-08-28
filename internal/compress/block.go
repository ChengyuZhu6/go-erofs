package compress

// 压缩相关常量
const (
	// 压缩块最大大小
	MaxBlockSize = 1024 * 1024 // 1MB

	// 压缩头部大小
	HeaderSize = 4

	// 压缩块标志
	BlockCompressed = 1
)

// BlockInfo 存储压缩块的信息
type BlockInfo struct {
	// 逻辑地址和长度
	LogicalAddr int64
	LogicalLen  int64

	// 物理地址和长度
	PhysicalAddr int64
	PhysicalLen  int64

	// 压缩算法
	Algorithm uint32

	// 块内偏移
	BlockOffset int64
}

// ParseBlockHeader 解析压缩块头部
func ParseBlockHeader(header []byte) (isCompressed bool, size int64, err error) {
	if len(header) < HeaderSize {
		return false, 0, ErrCorruptedData
	}

	// 解析头部
	headerValue := uint32(header[0]) | uint32(header[1])<<8 |
		uint32(header[2])<<16 | uint32(header[3])<<24

	// 检查是否为压缩块
	isCompressed = (headerValue & BlockCompressed) != 0

	// 获取大小
	if isCompressed {
		// 对于压缩块，大小存储在头部
		size = int64((headerValue >> 1) & 0x7FFFFFFF)
	} else {
		// 对于未压缩块，大小为0（表示使用默认块大小）
		size = 0
	}

	return
}
