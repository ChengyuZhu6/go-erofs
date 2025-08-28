package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/erofs/go-erofs"
)

func main() {
	var (
		imgPath     string
		extractPath string
		verbose     bool
		testRead    bool
	)

	flag.StringVar(&imgPath, "img", "", "Path to compressed erofs image")
	flag.StringVar(&extractPath, "extract", "", "Extract files to this directory")
	flag.BoolVar(&verbose, "v", false, "Verbose output")
	flag.BoolVar(&testRead, "test-read", false, "Test reading compressed files")
	flag.Parse()

	if imgPath == "" {
		log.Fatal("Please specify an erofs image with -img")
	}

	// 检查文件是否存在
	if _, err := os.Stat(imgPath); os.IsNotExist(err) {
		log.Fatalf("Image file %s does not exist", imgPath)
	}

	fmt.Printf("Testing compressed EROFS image: %s\n", imgPath)

	// 打开EROFS镜像
	f, err := os.Open(imgPath)
	if err != nil {
		log.Fatalf("Failed to open image: %v", err)
	}
	defer f.Close()

	// 创建EROFS文件系统
	img, err := erofs.EroFS(f)
	if err != nil {
		log.Fatalf("Failed to create EROFS filesystem: %v", err)
	}

	fmt.Println("✓ Successfully opened EROFS image")

	// 统计信息
	var totalFiles, totalDirs, compressedFiles int
	var totalSize int64
	var algorithmStats = make(map[uint8]int)

	// 遍历文件系统
	err = fs.WalkDir(img, "/", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if verbose {
				fmt.Printf("Error visiting %s: %v\n", path, err)
			}
			return err
		}

		// 跳过根目录
		if path == "/" {
			return nil
		}

		// 获取文件信息
		fi, err := entry.Info()
		if err != nil {
			if verbose {
				fmt.Printf("Error getting info for %s: %v\n", path, err)
			}
			return err
		}

		// 获取EROFS特定的信息
		st, ok := fi.Sys().(*erofs.Stat)
		if ok {
			var layoutStr string
			var algorithmStr string

			switch st.InodeLayout {
			case 0:
				layoutStr = "FlatPlain"
			case 1:
				layoutStr = "CompressedFull"
				compressedFiles++
			case 2:
				layoutStr = "FlatInline"
			case 3:
				layoutStr = "CompressedCompact"
				compressedFiles++
			case 4:
				layoutStr = "ChunkBased"
			default:
				layoutStr = fmt.Sprintf("Unknown(%d)", st.InodeLayout)
			}

			// 获取压缩算法信息
			switch st.InodeLayout {
			case 1, 3: // 压缩布局
				algorithmStr = getAlgorithmName(st.InodeLayout)
				algorithmStats[st.InodeLayout]++
			}

			if verbose {
				fmt.Printf("%s [%s] Size: %d, Layout: %s", path, entry.Type(), fi.Size(), layoutStr)
				if algorithmStr != "" {
					fmt.Printf(", Algorithm: %s", algorithmStr)
				}
				fmt.Println()
			}

			// 如果是压缩文件且需要测试读取
			if testRead && (st.InodeLayout == 1 || st.InodeLayout == 3) {
				if err := testCompressedFile(img, path, fi.Size()); err != nil {
					fmt.Printf("Warning: Failed to read compressed file %s: %v\n", path, err)
				} else if verbose {
					fmt.Printf("  ✓ Successfully read compressed file: %s\n", path)
				}
			}
		}

		if entry.IsDir() {
			totalDirs++
		} else {
			totalFiles++
			totalSize += fi.Size()
		}

		return nil
	})

	if err != nil {
		log.Fatalf("Error walking filesystem: %v", err)
	}

	// 打印统计信息
	fmt.Printf("\n=== EROFS Image Statistics ===\n")
	fmt.Printf("Total directories: %d\n", totalDirs)
	fmt.Printf("Total files: %d\n", totalFiles)
	fmt.Printf("Compressed files: %d\n", compressedFiles)
	fmt.Printf("Total size: %d bytes (%.2f MB)\n", totalSize, float64(totalSize)/1024/1024)

	// 打印压缩算法统计
	if len(algorithmStats) > 0 {
		fmt.Printf("\n=== Compression Algorithm Statistics ===\n")
		for layout, count := range algorithmStats {
			fmt.Printf("%s: %d files\n", getAlgorithmName(uint8(layout)), count)
		}
	}

	// 如果需要提取文件
	if extractPath != "" {
		fmt.Printf("\nExtracting files to: %s\n", extractPath)
		if err := extractFiles(img, extractPath); err != nil {
			log.Fatalf("Failed to extract files: %v", err)
		}
		fmt.Println("✓ Extraction completed successfully")
	}

	fmt.Println("\n✓ Compressed EROFS image test completed successfully")
}

// getAlgorithmName 获取压缩算法名称
func getAlgorithmName(layout uint8) string {
	switch layout {
	case 1:
		return "CompressedFull"
	case 3:
		return "CompressedCompact"
	default:
		return "Unknown"
	}
}

// testCompressedFile 测试读取压缩文件
func testCompressedFile(img fs.FS, path string, size int64) error {
	file, err := img.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	// 读取文件内容
	buf := make([]byte, size)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return err
	}

	if int64(n) != size {
		return fmt.Errorf("read size mismatch: got %d, want %d", n, size)
	}

	return nil
}

// extractFiles 提取文件到指定目录
func extractFiles(img fs.FS, extractPath string) error {
	return fs.WalkDir(img, "/", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// 跳过根目录
		if path == "/" {
			return nil
		}

		// 获取文件信息
		fi, err := entry.Info()
		if err != nil {
			return err
		}

		// 如果是目录，创建对应的目录
		if entry.IsDir() {
			extractDir := filepath.Join(extractPath, path)
			return os.MkdirAll(extractDir, 0755)
		}

		// 如果是文件，提取文件
		srcFile, err := img.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		// 创建目标文件
		dstPath := filepath.Join(extractPath, path)
		dstDir := filepath.Dir(dstPath)
		if err := os.MkdirAll(dstDir, 0755); err != nil {
			return err
		}

		dstFile, err := os.Create(dstPath)
		if err != nil {
			return err
		}
		defer dstFile.Close()

		// 复制文件内容
		if _, err := io.Copy(dstFile, srcFile); err != nil {
			return err
		}

		// 设置文件权限
		return os.Chmod(dstPath, fi.Mode().Perm())
	})
}
