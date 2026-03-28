package libfs

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// ──────────────────────────────────────────────────────────────────────────────
// 路径工具
// ──────────────────────────────────────────────────────────────────────────────

// LastSegment 取路径最后一段（文件名或目录名）
func LastSegment(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimRight(path, "/")
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return path
	}
	return path[idx+1:]
}

// ──────────────────────────────────────────────────────────────────────────────
// 文件 MD5
// ──────────────────────────────────────────────────────────────────────────────

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

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ──────────────────────────────────────────────────────────────────────────────
// SQL 查询构建器
// ──────────────────────────────────────────────────────────────────────────────

type sqlCondition struct {
	key   string
	value interface{}
	op    string
}

func sqlBuildQuery(table string, conditions []sqlCondition) (string, []interface{}) {
	var sb strings.Builder
	sb.WriteString("SELECT * FROM ")

	sb.WriteString(table)

	if len(conditions) > 0 {
		sb.WriteString(" WHERE")
	}

	args := make([]interface{}, 0, len(conditions))

	for _, c := range conditions {
		if c.op == "" {
			c.op = "="
		}
		sb.WriteString(fmt.Sprintf(" %s %s ? AND", c.key, c.op))
		args = append(args, c.value)
	}

	if len(conditions) > 0 {
		sb.WriteString(" 1=1;")
	}

	return sb.String(), args
}
