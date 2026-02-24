package libfs

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"
)

type MailText struct {
	filemd5    string
	blockmd5   string
	filesize   int64
	blocksize  int64
	createtime time.Time
	owner      string
	localpath  string
	mailfolder string
}

func MailTextToByte(mailText *MailText) []byte {
	s := ""
	s += fmt.Sprintf("filemd5:%v\n", mailText.filemd5)
	s += fmt.Sprintf("blockmd5:%v\n", mailText.blockmd5)
	s += fmt.Sprintf("filesize:%v\n", mailText.filesize)
	s += fmt.Sprintf("blocksize:%v\n", mailText.blocksize)
	s += fmt.Sprintf("createtime:%v\n", mailText.createtime.Format("2006-01-02 15:04:05"))
	s += fmt.Sprintf("owner:%v\n", mailText.owner)
	s += fmt.Sprintf("localpath:%v\n", mailText.localpath)
	s += fmt.Sprintf("mailfolder:%v\n", mailText.mailfolder)

	return []byte(s)
}

func md5File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hash := md5.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}

	// 计算 MD5 并转换为十六进制字符串
	return hex.EncodeToString(hash.Sum(nil)), nil
}
