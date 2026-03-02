package libfs

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// ──────────────────────────────────────────────────────────────────────────────
// 进度回调类型
// ──────────────────────────────────────────────────────────────────────────────

// BlockProgressFunc 块级进度回调: (当前块序号, 总块数, 当前文件名)
type BlockProgressFunc func(currentBlock, totalBlocks int64, fileName string)

// FileProgressFunc 文件级进度回调（用于目录上传）: (已处理文件数, 总文件数, 当前文件路径)
type FileProgressFunc func(doneFiles, totalFiles int, filePath string)

// ──────────────────────────────────────────────────────────────────────────────
// UploadFileWithProgress — 带块级进度回调的文件上传
// ──────────────────────────────────────────────────────────────────────────────

func (mailfs *MailFileSystem) UploadFileWithProgress(path string, blockCb BlockProgressFunc) error {
	if mailfs.c == nil {
		return errors.New("not login")
	}
	if len(mailfs.remoteDir) <= 0 {
		return errors.New("not select dir")
	}

	logrus.Infof("remote: %v, upload file: %v", mailfs.remoteDir, path)

	existed, err := isFileExisted(mailfs.remoteDir, path)
	if err != nil {
		return err
	}
	if existed {
		logrus.Infof("ignore..., file has existed: %v", path)
		if blockCb != nil {
			blockCb(1, 1, filepath.Base(path)) // 已存在视为完成
		}
		return nil
	}

	filemd5, err := md5File(path)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_RDONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	fInfo, err := f.Stat()
	if err != nil {
		return err
	}

	var fSize int64 = fInfo.Size()
	var fBlockCount int64 = fSize / FileBlockSize
	if fSize%FileBlockSize != 0 {
		fBlockCount++
	}

	fileName := filepath.Base(path)
	fileBlock := make([]byte, FileBlockSize)

	for i := int64(1); i <= fBlockCount; i++ {
		n, err := f.Read(fileBlock)
		if err != nil {
			return err
		}
		if n < FileBlockSize {
			fileBlock = fileBlock[:n]
		}

		md5byte := md5.Sum(fileBlock)
		blockmd5 := hex.EncodeToString(md5byte[:])

		header, err := mailfs.GenHeader(fileName, i, fBlockCount)
		if err != nil {
			return err
		}

		mailText := MailText{
			Vfilemd5:    filemd5,
			Vblockmd5:   blockmd5,
			Vfilesize:   fSize,
			Vblocksize:  int64(len(fileBlock)),
			Vcreatetime: time.Now(),
			Vowner:      "sunshine",
			Vlocalpath:  path,
			Vmailfolder: mailfs.remoteDir,
		}
		mailText.Vsubject, err = header.Subject()
		if err != nil {
			return err
		}

		if err = mailfs.UploadFileEach(header, MailTextToByte(&mailText), fileName, fileBlock); err != nil {
			return err
		}

		// 触发块进度回调
		if blockCb != nil {
			blockCb(i, fBlockCount, fileName)
		}

		// 重置 slice 容量（fileBlock 可能被截断）
		fileBlock = fileBlock[:FileBlockSize]
	}

	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// UploadDirWithProgress — 带双层进度回调的目录上传
// ──────────────────────────────────────────────────────────────────────────────

func (mailfs *MailFileSystem) UploadDirWithProgress(
	path string,
	fileCb FileProgressFunc,
	blockCb BlockProgressFunc,
) error {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fileInfo.IsDir() {
		return errors.New("not a dir")
	}

	// 先收集文件列表，计算总数
	var files []string
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			abs, err := filepath.Abs(p)
			if err == nil {
				files = append(files, filepath.ToSlash(abs))
			}
		}
		return nil
	})

	total := len(files)
	for i, fp := range files {
		if fileCb != nil {
			fileCb(i, total, fp)
		}
		if err := mailfs.UploadFileWithProgress(fp, blockCb); err != nil {
			logrus.Errorf("UploadFileWithProgress error: %v", err)
			// 继续上传其他文件，不中断
		}
	}
	if fileCb != nil {
		fileCb(total, total, "")
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// DownloadCacheFileWithProgress — 带块级进度回调的文件下载
// ──────────────────────────────────────────────────────────────────────────────

func (mailfs *MailFileSystem) DownloadCacheFileWithProgress(f CacheFile, blockCb BlockProgressFunc) error {
	if mailfs.c == nil {
		return errors.New("not login")
	}

	var err error

	if mailfs.remoteDir != f.MailFolder {
		if err = mailfs.Enter(f.MailFolder); err != nil {
			return fmt.Errorf("can't enter dir: %w", err)
		}
	}

	if f.Blocks == nil {
		if f.Blocks, err = getCacheBlockFromDB(f.FileID); err != nil {
			return fmt.Errorf("getCacheBlockFromDB error: %w", err)
		}
	}

	if int64(len(f.Blocks)) != f.BlockCount {
		return fmt.Errorf("block count mismatch: want %d, got %d", f.BlockCount, len(f.Blocks))
	}

	p := strings.Split(f.LocalPath, "/")
	p[0] = p[0][:1]
	savePath := strings.Join(p, "/")
	savePath = mailfs.downloadRootDir + savePath
	saveTmpPath := savePath + ".tmp"

	if _, err = os.Stat(savePath); err == nil {
		logrus.Debugf("file exist, %v", savePath)
		return nil
	}

	os.Remove(saveTmpPath)

	filename := filepath.Base(savePath)
	dir := filepath.Dir(savePath)
	fileCachePath := dir + "/mailfscache_" + f.FileMD5

	logrus.Infof("%s %s", filename, dir)

	if err = os.MkdirAll(fileCachePath, 0755); err != nil {
		return err
	}

	totalBlocks := int64(len(f.Blocks))

	for idx, block := range f.Blocks {
		cacheBlockPath := fileCachePath + "/" + strconv.FormatInt(block.BlockSeq, 10)

		if _, err = os.Stat(cacheBlockPath); err == nil {
			logrus.Debugf("block exist, seq: %v", block.BlockSeq)
			if blockCb != nil {
				blockCb(int64(idx+1), totalBlocks, filename)
			}
			continue
		}

		tmp := cacheBlockPath + ".tmp"
		os.Remove(tmp)

		mailText, b, err := mailfs.downloadUID(block.UID)
		if err != nil {
			return err
		}

		md5byte := md5.Sum(b)
		blockmd5 := hex.EncodeToString(md5byte[:])
		if block.BlockMD5 != blockmd5 {
			return errors.New("block md5 not match")
		}
		if mailText.Vmailfolder != f.MailFolder {
			return errors.New("mailfolder not match")
		}
		if mailText.Vlocalpath != f.LocalPath {
			return errors.New("localpath not match")
		}

		tmpf, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		tmpf.Write(b)
		tmpf.Close()

		if err = os.Rename(tmp, cacheBlockPath); err != nil {
			return err
		}

		if blockCb != nil {
			blockCb(int64(idx+1), totalBlocks, filename)
		}
	}

	// 合并块
	tmpf, err := os.OpenFile(saveTmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	for _, block := range f.Blocks {
		cacheBlockPath := fileCachePath + "/" + strconv.FormatInt(block.BlockSeq, 10)
		cb, err := os.OpenFile(cacheBlockPath, os.O_RDONLY, 0644)
		if err != nil {
			tmpf.Close()
			return err
		}
		blockdata, err := io.ReadAll(cb)
		cb.Close()
		if err != nil {
			tmpf.Close()
			return err
		}
		if _, err = tmpf.Write(blockdata); err != nil {
			tmpf.Close()
			return err
		}
	}
	tmpf.Close()

	filemd5, _ := md5File(saveTmpPath)
	if filemd5 != f.FileMD5 {
		return errors.New("file md5 not match")
	}

	if err = os.Rename(saveTmpPath, savePath); err != nil {
		return err
	}

	os.RemoveAll(fileCachePath)
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// CheckIntegrity — 检查文件完整性：比对 blockcount 与实际缓存块数
// ──────────────────────────────────────────────────────────────────────────────

// IntegrityResult 单个文件的完整性检查结果
type IntegrityResult struct {
	File           CacheFile
	CachedBlocks   int64 // 数据库中实际缓存的块数
	ExpectedBlocks int64 // 应有的块数（BlockCount）
	OK             bool
}

func CheckIntegrity(folder string) ([]IntegrityResult, error) {
	files, err := getCacheFileFromDB(folder)
	if err != nil {
		return nil, err
	}

	results := make([]IntegrityResult, 0, len(files))
	for _, f := range files {
		blocks, err := getCacheBlockFromDB(f.FileID)
		if err != nil {
			return nil, err
		}
		r := IntegrityResult{
			File:           f,
			CachedBlocks:   int64(len(blocks)),
			ExpectedBlocks: f.BlockCount,
			OK:             int64(len(blocks)) == f.BlockCount,
		}
		results = append(results, r)
	}
	return results, nil
}
