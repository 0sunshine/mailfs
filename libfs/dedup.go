package libfs

import (
	"errors"
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/sirupsen/logrus"
)

// DedupProgressFunc 去重进度回调: (已删除数, 总重复数)
type DedupProgressFunc func(done, total int)

// RemoveDuplicates 删除当前目录中的重复邮件。
//
// 原理: 重复上传会导致同一个 fileid+blockseq 在 IMAP 上有多个 UID，
// 但数据库 cache_blocks 表 UNIQUE(fileid, blockseq) 约束只保留了最后一次写入的 UID。
// 因此 IMAP 上存在但数据库中无记录的 UID 即为重复/废弃邮件，可安全删除。
//
// 调用前应先完成缓存同步（CacheCurrDirWithProgress），以确保数据库 UID 是最新的。
// 返回实际删除的邮件数量。
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

	// ── 3. 找出 IMAP 有但数据库没有的 UID（重复/废弃） ──
	var orphanUIDs []imap.UID
	for _, uid := range allUIDs {
		if !cachedSet[int64(uid)] {
			orphanUIDs = append(orphanUIDs, uid)
		}
	}

	total := len(orphanUIDs)
	logrus.Infof("去重: IMAP 共 %d 封, 数据库 %d 条, 发现 %d 个重复",
		len(allUIDs), len(cachedSet), total)

	if total == 0 {
		if progressCb != nil {
			progressCb(0, 0)
		}
		return 0, nil
	}

	// ── 4. 批量标记 \Deleted ─────────────────────────
	const batchSize = 100
	deleted := 0
	for i := 0; i < total; i += batchSize {
		end := i + batchSize
		if end > total {
			end = total
		}
		batch := orphanUIDs[i:end]

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
			return deleted, fmt.Errorf("标记删除失败: %w", err)
		}

		deleted += len(batch)
		if progressCb != nil {
			progressCb(deleted, total)
		}
	}

	// ── 5. EXPUNGE 永久删除 ──────────────────────────
	err = mailfs.withRetry("Dedup/EXPUNGE", func() error {
		return mailfs.imapDoTimeout("Dedup/EXPUNGE", func() error {
			return mailfs.c.Expunge().Close()
		})
	})
	if err != nil {
		return deleted, fmt.Errorf("EXPUNGE 失败（已标记 %d 封）: %w", deleted, err)
	}

	logrus.Infof("去重完成，共删除 %d 个重复邮件", deleted)
	return deleted, nil
}
