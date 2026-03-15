package libfs

import (
	"os"
	"strings"

	"github.com/sirupsen/logrus"
)

const foldersConfigPath = "folders.txt"

// LoadAllowedFolders 读取 folders.txt 配置文件，返回允许显示的目录集合。
//
// 文件格式：每行一个邮箱远程目录名，空行和 # 开头的注释行会被忽略。
// 示例：
//
//	# 只显示以下目录
//	其他文件夹/工作资料
//	其他文件夹/.私密
//	其他文件夹/备份
//
// 返回值：
//   - map 含条目：只显示 map 中存在的目录
//   - map 为空（文件不存在/为空/全是注释）：不显示任何目录
func LoadAllowedFolders() map[string]bool {
	b, err := os.ReadFile(foldersConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			logrus.Warnf("配置文件 %s 不存在，不显示任何目录", foldersConfigPath)
		} else {
			logrus.Warnf("读取配置文件 %s 失败: %v，不显示任何目录", foldersConfigPath, err)
		}
		return make(map[string]bool)
	}

	lines := strings.Split(string(b), "\n")
	allowed := make(map[string]bool)

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)

		// 跳过空行和注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		allowed[line] = true
	}

	if len(allowed) == 0 {
		logrus.Warnf("配置文件 %s 为空，不显示任何目录", foldersConfigPath)
	} else {
		logrus.Infof("已加载 %d 个允许显示的目录", len(allowed))
	}

	return allowed
}

// FilterFolders 根据允许列表过滤目录。
// 只保留 allowed 中存在的目录，allowed 为空则返回空列表。
func FilterFolders(folders []string, allowed map[string]bool) []string {
	filtered := make([]string, 0, len(allowed))
	for _, f := range folders {
		if allowed[f] {
			filtered = append(filtered, f)
		}
	}
	return filtered
}
