package libfs

import (
	"fmt"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	cases := []string{
		"hello.txt",
		"中文文件名.mp4",
		"/home/user/.secret/视频资料.avi",
		"C:/Users/test/.hidden/日记本.docx",
		"abc123!@#$%^&*()",
		"混合mixed中英文path/file文件.txt",
		"",
	}

	for _, s := range cases {
		enc := Encrypt(s)
		dec := Decrypt(enc)
		if dec != s {
			t.Errorf("RoundTrip failed: %q -> %q -> %q", s, enc, dec)
		}
		// 非空字符串加密后应完全不同
		if s != "" && enc == s {
			t.Errorf("Encrypt did not change: %q", s)
		}
		// 加密结果应该只含 hex 字符
		for _, c := range enc {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("Encrypt result contains non-hex char %c in %q", c, enc)
				break
			}
		}
	}
}

func TestEncryptHidesAllContent(t *testing.T) {
	// 验证中文完全不可见
	input := "这是一个中文路径/秘密文件夹/重要文件.pdf"
	enc := Encrypt(input)
	// 加密结果中不应出现任何中文字符
	for _, r := range enc {
		if r > 127 {
			t.Errorf("Encrypted result still contains non-ASCII char: %c", r)
		}
	}
	t.Logf("原文: %s", input)
	t.Logf("密文: %s", enc)
}

func TestNeedEncryptByRemoteDir(t *testing.T) {
	cases := []struct {
		remoteDir string
		expect    bool
	}{
		{"其他文件夹/.secret", true},
		{"其他文件夹/public", false},
		{"其他文件夹/a/.hidden", true},
		{"其他文件夹/.私密文件夹", true},
		{"INBOX", false},
		{"", false},
		{"其他文件夹/.dot", true},
		{"其他文件夹/normal", false},
	}

	for _, c := range cases {
		got := NeedEncryptByRemoteDir(c.remoteDir)
		if got != c.expect {
			t.Errorf("NeedEncryptByRemoteDir(%q) = %v, want %v", c.remoteDir, got, c.expect)
		}
	}
}

func TestIsEncryptedSubject(t *testing.T) {
	cases := []struct {
		subject string
		expect  bool
	}{
		{"file.txt/plain/1-5", false},
		{"a1b2c3d4e5/encrypted/1-5", true},
		{"file.txt/encrypted/2-10", true},
		{"bad-format", false},
	}

	for _, c := range cases {
		got := IsEncryptedSubject(c.subject)
		if got != c.expect {
			t.Errorf("IsEncryptedSubject(%q) = %v, want %v", c.subject, got, c.expect)
		}
	}
}

func TestDecryptSubject(t *testing.T) {
	fileName := "测试视频.mp4"
	encName := Encrypt(fileName)
	subject := fmt.Sprintf("%s/encrypted/3-10", encName)

	decSubject := DecryptSubject(subject)
	expected := fmt.Sprintf("%s/encrypted/3-10", fileName)
	if decSubject != expected {
		t.Errorf("DecryptSubject: got %q, want %q", decSubject, expected)
	}
}

func TestEncryptExamples(t *testing.T) {
	examples := []string{
		"hello.txt",
		"视频.mp4",
		"C:/Users/阳光/.secret/日记.docx",
	}
	for _, s := range examples {
		enc := Encrypt(s)
		t.Logf("原文: %-40s → 密文: %s", s, enc)
	}
}
