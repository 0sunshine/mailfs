package libfs

import (
	"bytes"
	"errors"
	"io"
	"mime/quotedprintable"
	"sync"
	"sync/atomic"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/sirupsen/logrus"
)

// CacheProgressFunc 同步缓存进度回调: (已完成UID数, 总UID数)
type CacheProgressFunc func(done, total int)

// CacheCurrDir 同步当前目录的所有邮件到本地缓存（无进度回调）
func (mailfs *MailFileSystem) CacheCurrDir() error {
	return mailfs.CacheCurrDirWithProgress(nil)
}

// CacheCurrDirWithProgress 同步当前目录的所有邮件到本地缓存，通过 progressCb 报告进度。
// 内部使用多个 IMAP 连接并发处理以提高速度。
func (mailfs *MailFileSystem) CacheCurrDirWithProgress(progressCb CacheProgressFunc) error {
	if mailfs.c == nil {
		return errors.New("not login")
	}

	if len(mailfs.remoteDir) <= 0 {
		return errors.New("not select dir")
	}

	// ── 1. 搜索所有 UID ──────────────────────────────
	var allUIDs []imap.UID
	err := mailfs.withRetry("CacheCurrDir/SEARCH", func() error {
		return mailfs.imapDoTimeout("UIDSearch/Wait", func() error {
			uids, err := mailfs.c.UIDSearch(&imap.SearchCriteria{
				Header: []imap.SearchCriteriaHeaderField{},
			}, nil).Wait()
			if err != nil {
				logrus.Errorf("UID SEARCH command failed: %v", err)
				return err
			}
			allUIDs = uids.AllUIDs()
			return nil
		})
	})
	if err != nil {
		return err
	}

	total := len(allUIDs)
	logrus.Infof("UID SEARCH 返回 %d 个 UID", total)

	if progressCb != nil {
		progressCb(0, total)
	}

	// ── 2. 批量加载已缓存 UID，筛选未缓存 ───────────
	cachedSet, err := getCachedUIDSet(mailfs.remoteDir)
	if err != nil {
		return err
	}
	logrus.Infof("已缓存 %d 个 UID，开始筛选未缓存…", len(cachedSet))

	var uncachedUIDs []imap.UID
	for _, uid := range allUIDs {
		if cachedSet[int64(uid)] {
			continue
		}
		uncachedUIDs = append(uncachedUIDs, uid)
	}

	cachedCount := total - len(uncachedUIDs)
	logrus.Infof("跳过 %d 个已缓存，需处理 %d 个未缓存", cachedCount, len(uncachedUIDs))

	if len(uncachedUIDs) == 0 {
		if progressCb != nil {
			progressCb(total, total)
		}
		return nil
	}

	// ── 3. 分批，每批最多 32 个 UID ─────────────────
	const batchSize = 32
	var batches [][]imap.UID
	for i := 0; i < len(uncachedUIDs); i += batchSize {
		end := i + batchSize
		if end > len(uncachedUIDs) {
			end = len(uncachedUIDs)
		}
		batches = append(batches, uncachedUIDs[i:end])
	}

	// ── 4. 创建 worker 连接（共 3 个：主连接 + 2 个额外）
	const workerCount = 3
	workers := make([]*MailFileSystem, workerCount)
	workers[0] = mailfs
	actualWorkers := 1
	for i := 1; i < workerCount; i++ {
		w := &MailFileSystem{cfg: mailfs.cfg}
		if err := w.Login(mailfs.usr, mailfs.pwd); err != nil {
			logrus.Warnf("worker %d 登录失败: %v，使用较少并发", i, err)
			break
		}
		if err := w.Enter(mailfs.remoteDir); err != nil {
			logrus.Warnf("worker %d 进入目录失败: %v，使用较少并发", i, err)
			w.safeLogout()
			break
		}
		workers[i] = w
		actualWorkers++
	}
	logrus.Infof("缓存同步使用 %d 个并发连接，共 %d 批", actualWorkers, len(batches))

	defer func() {
		for i := 1; i < actualWorkers; i++ {
			if workers[i] != nil {
				workers[i].safeLogout()
			}
		}
	}()

	// ── 5. 分发批次并并发处理 ────────────────────────
	batchCh := make(chan []imap.UID, len(batches))
	for _, batch := range batches {
		batchCh <- batch
	}
	close(batchCh)

	var (
		processed int64
		mu        sync.Mutex
		firstErr  error
		wg        sync.WaitGroup
	)

	for i := 0; i < actualWorkers; i++ {
		wg.Add(1)
		go func(w *MailFileSystem) {
			defer wg.Done()
			for batch := range batchCh {
				mu.Lock()
				failed := firstErr != nil
				mu.Unlock()
				if failed {
					return
				}

				if err := w.cacheUID(batch); err != nil {
					logrus.Errorf("cacheUID failed: %v", err)
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}

				n := atomic.AddInt64(&processed, int64(len(batch)))
				if progressCb != nil {
					progressCb(cachedCount+int(n), total)
				}
			}
		}(workers[i])
	}

	wg.Wait()

	return firstErr
}

// cacheUID 批量 FETCH 指定 UID 列表的邮件正文，解析元数据并写入本地数据库。
func (mailfs *MailFileSystem) cacheUID(uids []imap.UID) error {
	logrus.Debugf("cacheUID : %v", uids)

	var mailText *MailText
	var msgUID imap.UID
	var messages []*imapclient.FetchMessageBuffer
	bodySection := &imap.FetchItemBodySection{Part: []int{1}}

	err := mailfs.withRetry("cacheUID/FETCH", func() error {
		uidSeqSet := imap.UIDSetNum(uids...)
		fetchOptions := &imap.FetchOptions{
			UID:         true,
			BodySection: []*imap.FetchItemBodySection{bodySection},
		}

		logrus.Debugf("FETCH begin...")

		return mailfs.imapDoTimeout("cacheUID/FETCH", func() error {
			var err error
			messages, err = mailfs.c.Fetch(uidSeqSet, fetchOptions).Collect()
			if err != nil {
				logrus.Errorf("FETCH command failed: %v", err)
				return err
			}

			if len(messages) <= 0 {
				logrus.Errorf("len(messages) <= 0")
				return errors.New("len(messages) <= 0")
			}

			logrus.Debugf("FETCH end...")
			return nil
		})
	})

	if err != nil {
		return err
	}

	for _, msg := range messages {
		header := msg.FindBodySection(bodySection)
		r := quotedprintable.NewReader(bytes.NewReader(header))
		b, err := io.ReadAll(r)
		if err != nil {
			logrus.Errorf("io.ReadAll failed: %v", err)
			return err
		}

		mailText = MailTextFromByte(string(b))
		msgUID = msg.UID

		// 如果是加密邮件，解密 localpath 和 subject
		if IsEncryptedSubject(mailText.Subject) {
			mailText.LocalPath = Decrypt(mailText.LocalPath)
			mailText.Subject = DecryptSubject(mailText.Subject)
		}

		if len(mailText.Subject) == 0 || len(mailText.LocalPath) == 0 {
			logrus.Warnf("not a mailfs: %v, uid: %v", mailText.Subject, msgUID)
			continue
		}

		err = cacheToDB(msgUID, mailText)
		if err != nil {
			logrus.Errorf("cacheToDB failed: %v", err)
			return err
		}
	}

	return nil
}
