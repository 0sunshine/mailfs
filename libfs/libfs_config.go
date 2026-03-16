package libfs

import (
	"os"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const configPath = "config.yaml"

// AppConfig 统一配置结构
type AppConfig struct {
	// HTTP 流媒体服务监听地址
	HTTPListenAddr string `yaml:"http_listen_addr"`
	// 复制 HTTP 播放链接时使用的地址
	HTTPCopyAddr string `yaml:"http_copy_addr"`
	// 允许显示的邮箱远程目录列表
	AllowedFolders []string `yaml:"allowed_folders"`
	// 目录上传时忽略的文件后缀（不区分大小写）
	IgnoreExtensions []string `yaml:"ignore_extensions"`
}

var (
	globalConfig     *AppConfig
	globalConfigOnce sync.Once
)

// defaultConfig 返回默认配置
func defaultConfig() *AppConfig {
	return &AppConfig{
		HTTPListenAddr:   ":9867",
		HTTPCopyAddr:     "http://127.0.0.1:9867",
		AllowedFolders:   []string{},
		IgnoreExtensions: []string{},
	}
}

// LoadConfig 从 config.yaml 加载配置，仅加载一次。
// 后续调用返回缓存的配置。
func LoadConfig() *AppConfig {
	globalConfigOnce.Do(func() {
		globalConfig = loadConfigFromFile()
	})
	return globalConfig
}

// ReloadConfig 强制重新加载配置（用于测试或热更新场景）
func ReloadConfig() *AppConfig {
	globalConfigOnce = sync.Once{}
	return LoadConfig()
}

func loadConfigFromFile() *AppConfig {
	cfg := defaultConfig()

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			logrus.Warnf("配置文件 %s 不存在，使用默认配置", configPath)
		} else {
			logrus.Warnf("读取配置文件 %s 失败: %v，使用默认配置", configPath, err)
		}
		return cfg
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		logrus.Errorf("解析配置文件 %s 失败: %v，使用默认配置", configPath, err)
		return defaultConfig()
	}

	// 将忽略后缀统一转为小写
	for i, ext := range cfg.IgnoreExtensions {
		cfg.IgnoreExtensions[i] = strings.ToLower(ext)
	}

	logrus.Infof("已加载配置: listen=%s, copy=%s, folders=%d, ignore_ext=%d",
		cfg.HTTPListenAddr, cfg.HTTPCopyAddr,
		len(cfg.AllowedFolders), len(cfg.IgnoreExtensions))

	return cfg
}

// ──────────────────────────────────────────────────────────────────
// 向后兼容的辅助函数
// ──────────────────────────────────────────────────────────────────

// LoadAllowedFolders 从统一配置中返回允许显示的目录集合。
func LoadAllowedFolders() map[string]bool {
	cfg := LoadConfig()
	allowed := make(map[string]bool, len(cfg.AllowedFolders))
	for _, f := range cfg.AllowedFolders {
		f = strings.TrimSpace(f)
		if f != "" {
			allowed[f] = true
		}
	}
	if len(allowed) == 0 {
		logrus.Warnf("配置中未指定 allowed_folders，不显示任何目录")
	} else {
		logrus.Infof("已加载 %d 个允许显示的目录", len(allowed))
	}
	return allowed
}

// FilterFolders 根据允许列表过滤目录。
func FilterFolders(folders []string, allowed map[string]bool) []string {
	filtered := make([]string, 0, len(allowed))
	for _, f := range folders {
		if allowed[f] {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// ShouldIgnoreFile 判断文件是否应在目录上传时被忽略（基于后缀匹配）。
func ShouldIgnoreFile(fileName string) bool {
	cfg := LoadConfig()
	if len(cfg.IgnoreExtensions) == 0 {
		return false
	}
	lower := strings.ToLower(fileName)
	for _, ext := range cfg.IgnoreExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}
