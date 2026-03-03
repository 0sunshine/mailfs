package libfs

import (
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const maxRetries = 10

// isNetworkError 判断是否为网络相关错误（IMAP 连接断开、超时、EOF 等）
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	keywords := []string{
		"connection reset",
		"broken pipe",
		"timeout",
		"timed out",
		"connection refused",
		"network is unreachable",
		"i/o timeout",
		"use of closed network connection",
		"connection abort",
		"dial tcp",
	}
	for _, kw := range keywords {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// withRetry 对 fn 进行自动重试。发生网络错误时，Logout → 等待 → Login → 重试。
// 退避间隔：1s, 2s, 4s, 8s, ... 最多重试 maxRetries 次。
// 成功或遇到非网络错误时立即返回。
func (mailfs *MailFileSystem) withRetry(operation string, fn func() error) error {
	err := fn()
	if err == nil || !isNetworkError(err) {
		return err
	}

	backoff := time.Second // 初始 1s

	for attempt := 1; attempt <= maxRetries; attempt++ {
		logrus.Warnf("[重试 %d/%d] %s 网络错误: %v，%v 后重试…",
			attempt, maxRetries, operation, err, backoff)

		// 先断开旧连接
		mailfs.Logout()

		time.Sleep(backoff)

		// 重新登录
		if loginErr := mailfs.reLogin(); loginErr != nil {
			logrus.Errorf("[重试 %d/%d] 重新登录失败: %v", attempt, maxRetries, loginErr)
			err = loginErr
			backoff *= 2
			continue
		}

		// 如果之前选中了某个文件夹，重新 Enter
		if mailfs.remoteDir != "" {
			if enterErr := mailfs.Enter(mailfs.remoteDir); enterErr != nil {
				logrus.Errorf("[重试 %d/%d] 重新进入文件夹失败: %v", attempt, maxRetries, enterErr)
				err = enterErr
				backoff *= 2
				continue
			}
		}

		// 重试业务逻辑
		err = fn()
		if err == nil {
			logrus.Infof("[重试 %d/%d] %s 重试成功", attempt, maxRetries, operation)
			return nil
		}

		if !isNetworkError(err) {
			// 非网络错误，不再重试
			return err
		}

		backoff *= 2
	}

	return fmt.Errorf("%s 重试 %d 次后仍失败: %w", operation, maxRetries, err)
}

// reLogin 使用保存的凭据重新登录
func (mailfs *MailFileSystem) reLogin() error {
	if mailfs.usr == "" || mailfs.pwd == "" {
		return fmt.Errorf("无保存的登录凭据，无法自动重连")
	}
	return mailfs.Login(mailfs.usr, mailfs.pwd)
}
