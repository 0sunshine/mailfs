package libfs

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type MailText struct {
	Vsubject    string    `mailfs:"subject"`
	Vfilemd5    string    `mailfs:"filemd5"`
	Vblockmd5   string    `mailfs:"blockmd5"`
	Vfilesize   int64     `mailfs:"filesize"`
	Vblocksize  int64     `mailfs:"blocksize"`
	Vcreatetime time.Time `mailfs:"createtime"`
	Vowner      string    `mailfs:"owner"`
	Vlocalpath  string    `mailfs:"localpath"`
	Vmailfolder string    `mailfs:"mailfolder"`
}

func MailTextToByte(mailText *MailText) []byte {
	s := ""
	s += fmt.Sprintf("subject:%v\n", mailText.Vsubject)
	s += fmt.Sprintf("filemd5:%v\n", mailText.Vfilemd5)
	s += fmt.Sprintf("blockmd5:%v\n", mailText.Vblockmd5)
	s += fmt.Sprintf("filesize:%v\n", mailText.Vfilesize)
	s += fmt.Sprintf("blocksize:%v\n", mailText.Vblocksize)
	s += fmt.Sprintf("createtime:%v\n", mailText.Vcreatetime.Format(time.RFC3339))
	s += fmt.Sprintf("owner:%v\n", mailText.Vowner)
	s += fmt.Sprintf("localpath:%v\n", mailText.Vlocalpath)
	s += fmt.Sprintf("mailfolder:%v\n", mailText.Vmailfolder)

	return []byte(s)
}

func MailTextFromByte(s string) *MailText {

	o := MailText{}

	fields := strings.Split(s, "\r\n")
	mapFields := make(map[string]string)

	for _, each := range fields {
		if !strings.Contains(each, ":") {
			continue
		}

		pos := strings.IndexByte(each, ':')
		if pos == -1 {
			continue
		}

		mapFields[each[0:pos]] = each[pos+1:]
	}

	v := reflect.ValueOf(&o).Elem()

	for i := 0; i < v.NumField(); i++ {
		fieldInfo := v.Type().Field(i)
		tag := fieldInfo.Tag
		name := tag.Get("mailfs")
		if name == "" {
			continue
		}

		f, ok := mapFields[name]
		if !ok {
			continue
		}

		rv := v.Field(i)
		if !rv.CanSet() {
			continue
		}

		if rv.Type() == reflect.TypeOf(time.Time{}) {
			t, err := time.Parse(time.RFC3339, f)
			if err == nil {
				rv.Set(reflect.ValueOf(t))
			}
		}

		switch rv.Kind() {
		case reflect.String:
			rv.SetString(f)
		case reflect.Int64:
			n, err := strconv.ParseInt(f, 10, 64)
			if err == nil {
				rv.SetInt(n)
			}
		}
	}

	return &o
}

type CacheBlock struct {
	FileID   int64
	BlockSeq int64
	UID      int64
	BlockMD5 string
}

type CacheFile struct {
	FileID     int64
	MailFolder string
	LocalPath  string
	BlockCount int64
	FileMD5    string
	Blocks     []CacheBlock
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
