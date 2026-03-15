package libfs

import (
	"encoding/hex"
	"strings"
)

// ──────────────────────────────────────────────────────────────────────────────
// 简单加密/解密（XOR + Hex）
//
// 对原始字节逐字节与循环密钥做 XOR，然后 hex 编码输出。
// 所有字符（中文、英文、数字、特殊符号）都会变成纯十六进制字符串，
// 完全不可读，也无法直观猜出编码方式。
//
// 加密结果只含 0-9 a-f，不含 "/" 等特殊字符，可安全用于邮件主题。
// ──────────────────────────────────────────────────────────────────────────────

// xorKey 固定异或密钥，长度越长越难猜测
var xorKey = []byte{0xa3, 0x5f, 0xe1, 0x7c, 0x92, 0x4b, 0xd8, 0x06, 0xf7, 0x31, 0x6e, 0xb4}

// xorBytes 对数据逐字节与循环密钥异或
func xorBytes(data []byte) []byte {
	out := make([]byte, len(data))
	keyLen := len(xorKey)
	for i, b := range data {
		out[i] = b ^ xorKey[i%keyLen]
	}
	return out
}

// Encrypt 加密字符串：XOR → hex 编码
func Encrypt(s string) string {
	if s == "" {
		return ""
	}
	xored := xorBytes([]byte(s))
	return hex.EncodeToString(xored)
}

// Decrypt 解密字符串：hex 解码 → XOR（异或自身互逆）
func Decrypt(s string) string {
	if s == "" {
		return ""
	}
	xored, err := hex.DecodeString(s)
	if err != nil {
		// 解码失败，原样返回（兼容非加密数据）
		return s
	}
	raw := xorBytes(xored)
	return string(raw)
}

// ──────────────────────────────────────────────────────────────────────────────
// 判断是否需要加密（基于远程邮箱目录）
// ──────────────────────────────────────────────────────────────────────────────

// NeedEncryptByRemoteDir 判断远程邮箱目录是否需要加密：
// 取 remoteDir 最后一级目录名，若以 "." 开头则需要加密。
//
// 例：
//   "其他文件夹/.secret"   → 最后一级 ".secret"  → true
//   "其他文件夹/public"    → 最后一级 "public"   → false
//   "其他文件夹/a/.hidden" → 最后一级 ".hidden"  → true
func NeedEncryptByRemoteDir(remoteDir string) bool {
	if remoteDir == "" {
		return false
	}
	d := strings.ReplaceAll(remoteDir, "\\", "/")
	d = strings.TrimRight(d, "/")

	lastSlash := strings.LastIndex(d, "/")
	lastSeg := d
	if lastSlash >= 0 {
		lastSeg = d[lastSlash+1:]
	}
	return strings.HasPrefix(lastSeg, ".")
}

// ──────────────────────────────────────────────────────────────────────────────
// 邮件主题中的加密标识判断
// ──────────────────────────────────────────────────────────────────────────────

// IsEncryptedSubject 从邮件主题判断是否为加密邮件
// 主题格式: "fileName/encrypted/seq-count" 或 "fileName/plain/seq-count"
func IsEncryptedSubject(subject string) bool {
	parts := strings.Split(subject, "/")
	if len(parts) >= 3 {
		return parts[1] == "encrypted"
	}
	return false
}

// DecryptSubject 对加密的邮件主题进行解密，返回解密后的主题
// 输入: "hex密文/encrypted/1-5"
// 输出: "真实文件名/encrypted/1-5"
func DecryptSubject(subject string) string {
	parts := strings.Split(subject, "/")
	if len(parts) < 3 {
		return subject
	}
	if parts[1] != "encrypted" {
		return subject
	}
	// 只解密文件名部分（第一段）
	parts[0] = Decrypt(parts[0])
	return strings.Join(parts, "/")
}
