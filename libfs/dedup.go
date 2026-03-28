package libfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/sirupsen/logrus"
)

// DedupProgressFunc 去重进度回调: (已检查数, 总待检查数)
type DedupProgressFunc func(done, total int)

// blockKey 用于标识一个文件块（localpath + blockseq）
type blockKey struct {
	localpath string
	blockseq  int
}

// RemoveDuplicates 删除当前目录中的重复邮件。
//
// 流程:
//  1. UID SEARCH 获取 IMAP 上所有 UID
//  2. 过滤出数据库中没有记录的 UID（未缓存的）
//  3. 逐批 FETCH 这些 UID 的邮件正文，解析出 localpath 和 blockseq
//  4. 查询数据库: 如果该 localpath+blockseq 已经存在（被另一个 UID 占据），
//     则说明当前 UID 是重复上传产生的，标记删除
//  5. EXPUNGE 永久删除
//
// 不依赖事先同步缓存。返回实际删除的邮件数量。
func (mailfs *MailFileSystem) RemoveDuplicates(progressCb DedupProgressFunc) (int, error) {
	if mailfs.c == nil {
		return 0, errors.New("not login")
	}
	if mailfs.remoteDir == "" {
		return 0, errors.New("not select dir")
	}

	// ── 1. 获取 IMAP 上的所有 UID ────────────────────
	var allUIDs []imap.UID
	err := mailfs.withRetry("Dedup/SEARCH", func() error {
		return mailfs.imapDoTimeout("Dedup/SEARCH", func() error {
			result, err := mailfs.c.UIDSearch(&imap.SearchCriteria{
				Header: []imap.SearchCriteriaHeaderField{},
			}, nil).Wait()
			if err != nil {
				return err
			}
			allUIDs = result.AllUIDs()
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("UID SEARCH 失败: %w", err)
	}

	// ── 2. 获取数据库中该目录的所有已缓存 UID ──────────
	cachedSet, err := getCachedUIDSet(mailfs.remoteDir)
	if err != nil {
		return 0, fmt.Errorf("查询缓存 UID 失败: %w", err)
	}

	// ── 3. 过滤出未缓存的 UID ──────────────────────────
	var uncachedUIDs []imap.UID
	for _, uid := range allUIDs {
		if !cachedSet[int64(uid)] {
			uncachedUIDs = append(uncachedUIDs, uid)
		}
	}

	total := len(uncachedUIDs)
	logrus.Infof("去重: IMAP 共 %d 封, 已缓存 %d, 未缓存 %d 需检查",
		len(allUIDs), len(cachedSet), total)

	if total == 0 {
		if progressCb != nil {
			progressCb(0, 0)
		}
		return 0, nil
	}

	// ── 4. 加载数据库中已有的 (localpath, blockseq) 集合 ──
	existingBlocks, err := getExistingBlockKeys(mailfs.remoteDir)
	if err != nil {
		return 0, fmt.Errorf("加载已有块信息失败: %w", err)
	}
	logrus.Infof("去重: 数据库中已有 %d 个块记录", len(existingBlocks))

	// ── 5. 分批 FETCH 未缓存 UID，检查是否为重复 ────────
	const batchSize = 32
	var duplicateUIDs []imap.UID
	checked := 0

	for i := 0; i < total; i += batchSize {
		end := i + batchSize
		if end > total {
			end = total
		}
		batch := uncachedUIDs[i:end]

		dups, err := mailfs.checkDuplicateBatch(batch, existingBlocks)
		if err != nil {
			logrus.Errorf("去重: 检查批次失败: %v", err)
			// 继续下一批，不中断整个流程
		} else {
			duplicateUIDs = append(duplicateUIDs, dups...)
		}

		checked += len(batch)
		if progressCb != nil {
			progressCb(checked, total)
		}
	}

	if len(duplicateUIDs) == 0 {
		logrus.Infof("去重: 检查完毕，未发现重复邮件")
		return 0, nil
	}

	logrus.Infof("去重: 发现 %d 个重复邮件，开始删除…", len(duplicateUIDs))

	// ── 6. 批量标记 \Deleted ─────────────────────────
	const deleteBatchSize = 100
	for i := 0; i < len(duplicateUIDs); i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > len(duplicateUIDs) {
			end = len(duplicateUIDs)
		}
		batch := duplicateUIDs[i:end]

		err := mailfs.withRetry("Dedup/STORE", func() error {
			return mailfs.imapDoTimeout("Dedup/STORE", func() error {
				uidSet := imap.UIDSetNum(batch...)
				storeFlags := &imap.StoreFlags{
					Op:     imap.StoreFlagsAdd,
					Silent: true,
					Flags:  []imap.Flag{imap.FlagDeleted},
				}
				return mailfs.c.Store(uidSet, storeFlags, nil).Close()
			})
		})
		if err != nil {
			return 0, fmt.Errorf("标记删除失败: %w", err)
		}
	}

	// ── 7. EXPUNGE 永久删除 ──────────────────────────
	err = mailfs.withRetry("Dedup/EXPUNGE", func() error {
		return mailfs.imapDoTimeout("Dedup/EXPUNGE", func() error {
			return mailfs.c.Expunge().Close()
		})
	})
	if err != nil {
		return len(duplicateUIDs), fmt.Errorf("EXPUNGE 失败（已标记 %d 封）: %w", len(duplicateUIDs), err)
	}

	logrus.Infof("去重完成，共删除 %d 个重复邮件", len(duplicateUIDs))
	return len(duplicateUIDs), nil
}

// checkDuplicateBatch 批量 FETCH 一组 UID 的邮件正文，解析出 localpath+blockseq，
// 与 existingBlocks 比对，返回其中属于重复的 UID 列表。
func (mailfs *MailFileSystem) checkDuplicateBatch(
	uids []imap.UID,
	existingBlocks map[blockKey]bool,
) ([]imap.UID, error) {
	bodySection := &imap.FetchItemBodySection{Part: []int{1}}
	var messages []*imapclient.FetchMessageBuffer

	err := mailfs.withRetry("Dedup/FETCH", func() error {
		return mailfs.imapDoTimeout("Dedup/FETCH", func() error {
			uidSeqSet := imap.UIDSetNum(uids...)
			fetchOptions := &imap.FetchOptions{
				UID:         true,
				BodySection: []*imap.FetchItemBodySection{bodySection},
			}
			var err error
			messages, err = mailfs.c.Fetch(uidSeqSet, fetchOptions).Collect()
			return err
		})
	})
	if err != nil {
		return nil, err
	}

	var dups []imap.UID
	for _, msg := range messages {
		header := msg.FindBodySection(bodySection)
		if header == nil {
			continue
		}

		r := quotedprintable.NewReader(bytes.NewReader(header))
		b, err := io.ReadAll(r)
		if err != nil {
			logrus.Warnf("去重: 解析 UID %d 正文失败: %v", msg.UID, err)
			continue
		}

		mailText := MailTextFromByte(string(b))

		// 处理加密邮件
		if IsEncryptedSubject(mailText.Subject) {
			mailText.LocalPath = Decrypt(mailText.LocalPath)
			mailText.Subject = DecryptSubject(mailText.Subject)
		}

		if len(mailText.Subject) == 0 || len(mailText.LocalPath) == 0 {
			logrus.Warnf("去重: UID %d 不是 mailfs 邮件，跳过", msg.UID)
			continue
		}

		// 从 subject 解析 blockseq: 格式 "filename/mode/blockseq-blockcount"
		s := strings.Split(mailText.Subject, "/")
		if len(s) < 3 {
			continue
		}
		n := strings.Split(s[2], "-")
		if len(n) < 2 {
			continue
		}
		blockseq, err := strconv.Atoi(n[0])
		if err != nil {
			continue
		}

		// 检查该 localpath+blockseq 是否已在数据库中
		key := blockKey{localpath: mailText.LocalPath, blockseq: blockseq}
		if existingBlocks[key] {
			logrus.Debugf("去重: UID %d 是重复邮件 (localpath=%s, blockseq=%d)",
				msg.UID, mailText.LocalPath, blockseq)
			dups = append(dups, msg.UID)
		}
	}

	return dups, nil
}

// getExistingBlockKeys 从数据库加载指定目录下所有已缓存的 (localpath, blockseq) 组合。
func getExistingBlockKeys(mailfolder string) (map[blockKey]bool, error) {
	rows, err := db.Query(`
		SELECT f.localpath, b.blockseq FROM cache_blocks b
		INNER JOIN cache_files f ON b.fileid = f.fileid
		WHERE f.mailfolder = ?
	`, mailfolder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make(map[blockKey]bool, 4096)
	for rows.Next() {
		var lp string
		var seq int
		if err := rows.Scan(&lp, &seq); err != nil {
			return nil, err
		}
		keys[blockKey{localpath: lp, blockseq: seq}] = true
	}
	return keys, nil
}
