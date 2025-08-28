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
		path        string
		extractPath string
		listOnly    bool
	)

	flag.StringVar(&path, "img", "", "Path to erofs image")
	flag.StringVar(&extractPath, "extract", "", "Extract files to this directory")
	flag.BoolVar(&listOnly, "list", false, "Only list files without extracting")
	flag.Parse()

	if path == "" {
		log.Fatal("Please specify an erofs image with -img")
	}

	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	img, err := erofs.EroFS(f)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Found valid image...\n")

	// 如果指定了提取目录，创建它
	if extractPath != "" && !listOnly {
		if err := os.MkdirAll(extractPath, 0755); err != nil {
			log.Fatalf("Failed to create extraction directory: %v", err)
		}
	}

	err = fs.WalkDir(img, "/", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("error visiting %s: %w", path, err)
		}

		// 打印文件信息
		fi, err := entry.Info()
		if err != nil {
			return fmt.Errorf("error getting info for %s: %w", path, err)
		}

		// 获取EROFS特定的信息
		st, ok := fi.Sys().(*erofs.Stat)
		var layoutStr string
		if ok {
			switch st.InodeLayout {
			case 0:
				layoutStr = "FlatPlain"
			case 1:
				layoutStr = "CompressedFull"
			case 2:
				layoutStr = "FlatInline"
			case 3:
				layoutStr = "CompressedCompact"
			case 4:
				layoutStr = "ChunkBased"
			default:
				layoutStr = fmt.Sprintf("Unknown(%d)", st.InodeLayout)
			}
		}

		fmt.Printf("%s [%s] Size: %d, Layout: %s\n", path, entry.Type(), fi.Size(), layoutStr)

		// 如果是目录，创建对应的目录
		if entry.IsDir() && extractPath != "" && !listOnly {
			extractDir := filepath.Join(extractPath, path)
			if err := os.MkdirAll(extractDir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %v", extractDir, err)
			}
			return nil
		}

		// 如果需要提取文件
		if !entry.IsDir() && extractPath != "" && !listOnly {
			// 打开源文件
			srcFile, err := img.Open(path)
			if err != nil {
				return fmt.Errorf("failed to open %s: %v", path, err)
			}
			defer srcFile.Close()

			// 创建目标文件
			dstPath := filepath.Join(extractPath, path)
			dstDir := filepath.Dir(dstPath)
			if err := os.MkdirAll(dstDir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %v", dstDir, err)
			}

			dstFile, err := os.Create(dstPath)
			if err != nil {
				return fmt.Errorf("failed to create %s: %v", dstPath, err)
			}
			defer dstFile.Close()

			// 复制文件内容
			if _, err := io.Copy(dstFile, srcFile); err != nil {
				return fmt.Errorf("failed to extract %s: %v", path, err)
			}

			// 设置文件权限
			if err := os.Chmod(dstPath, fi.Mode().Perm()); err != nil {
				return fmt.Errorf("failed to set permissions for %s: %v", dstPath, err)
			}

			fmt.Printf("  Extracted: %s\n", dstPath)
		}

		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}
