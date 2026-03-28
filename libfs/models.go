package libfs

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// MailText — 邮件正文中的元数据（每封邮件携带一个块的信息）
// ──────────────────────────────────────────────────────────────────────────────

type MailText struct {
	Subject    string    `mailfs:"subject"`
	FileMD5    string    `mailfs:"filemd5"`
	BlockMD5   string    `mailfs:"blockmd5"`
	FileSize   int64     `mailfs:"filesize"`
	BlockSize  int64     `mailfs:"blocksize"`
	CreateTime time.Time `mailfs:"createtime"`
	Owner      string    `mailfs:"owner"`
	LocalPath  string    `mailfs:"localpath"`
	MailFolder string    `mailfs:"mailfolder"`
}

// MailTextToByte 将 MailText 序列化为 key:value 文本格式
func MailTextToByte(mt *MailText) []byte {
	s := ""
	s += fmt.Sprintf("subject:%v\n", mt.Subject)
	s += fmt.Sprintf("filemd5:%v\n", mt.FileMD5)
	s += fmt.Sprintf("blockmd5:%v\n", mt.BlockMD5)
	s += fmt.Sprintf("filesize:%v\n", mt.FileSize)
	s += fmt.Sprintf("blocksize:%v\n", mt.BlockSize)
	s += fmt.Sprintf("createtime:%v\n", mt.CreateTime.Format(time.RFC3339))
	s += fmt.Sprintf("owner:%v\n", mt.Owner)
	s += fmt.Sprintf("localpath:%v\n", mt.LocalPath)
	s += fmt.Sprintf("mailfolder:%v\n", mt.MailFolder)

	return []byte(s)
}

// MailTextFromByte 从 key:value 文本反序列化为 MailText
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

// ──────────────────────────────────────────────────────────────────────────────
// CacheFile / CacheBlock — 数据库缓存记录对应的内存模型
// ──────────────────────────────────────────────────────────────────────────────

type CacheBlock struct {
	FileID    int64
	BlockSeq  int64
	UID       int64
	BlockMD5  string
	BlockSize int64 // 该块的实际大小（字节）
}

type CacheFile struct {
	FileID     int64
	MailFolder string
	LocalPath  string
	BlockCount int64
	FileMD5    string
	FileSize   int64 // 文件总大小（字节）
	Blocks     []CacheBlock
}

// blockContains 检查文件的已缓存块中是否包含指定序号
func blockContains(f *CacheFile, seq int64) bool {
	if f == nil || f.Blocks == nil {
		return false
	}

	for _, v := range f.Blocks {
		if v.BlockSeq == seq {
			return true
		}
	}
	return false
}
