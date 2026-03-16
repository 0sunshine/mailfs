package libfs

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// ──────────────────────────────────────────────────────────────────────────────
// HTTPIMAPServer — 通过 HTTP 流式播放 IMAP 邮件附件
//
// 架构：生产者-消费者
//
//   HTTP 请求（消费者）：计算所需块 → 查缓存 → 命中直接返回 →
//       未命中则提交下载任务并等待（最多 60s）
//
//   下载调度器（生产者）：3 个 worker goroutine 从队列按序取任务 →
//       IMAP FETCH → 结果写入 LRU 缓存 → 通知等待者
//
//   取消联动：HTTP 断开 → 任务的 ctx 被取消 →
//       排队中的任务被跳过 / 正在下载的任务中止
// ──────────────────────────────────────────────────────────────────────────────

const (
	DefaultBlockCacheSize = 8
	MaxIMAPWorkers        = 3
	BlockWaitTimeout      = 60 * time.Second
)

// ──────────────────────────────────────────────────────────────────────────────
// downloadTask — 提交给 worker pool 的下载任务
// ──────────────────────────────────────────────────────────────────────────────

type downloadTask struct {
	imapDir string
	uid     int64
}

// ──────────────────────────────────────────────────────────────────────────────
// blockWaiter — 对同一个块的等待去重
//
// 多个 HTTP 请求可能同时需要同一个块（如播放器发探测请求+正式请求），
// 只需下载一次，所有等待者共享同一个 done channel。
// ──────────────────────────────────────────────────────────────────────────────

type blockWaiter struct {
	done    chan struct{}
	err     error
	refCtxs []context.Context // 所有等待该块的请求 ctx
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTPIMAPServer
// ──────────────────────────────────────────────────────────────────────────────

type HTTPIMAPServer struct {
	addr       string
	blockCache *BlockLRUCache

	taskQueue chan *downloadTask // 任务队列，worker 从此取任务

	waiterMu sync.Mutex
	waiters  map[string]*blockWaiter // key → 正在下载/排队中的块等待器
}

func NewHTTPIMAPServer(addr string, cacheBlocks ...int) *HTTPIMAPServer {
	n := DefaultBlockCacheSize
	if len(cacheBlocks) > 0 && cacheBlocks[0] > 0 {
		n = cacheBlocks[0]
	}

	srv := &HTTPIMAPServer{
		addr:       addr,
		blockCache: NewBlockLRUCache(n),
		taskQueue:  make(chan *downloadTask, 256),
		waiters:    make(map[string]*blockWaiter),
	}

	// 启动 worker pool
	for i := 0; i < MaxIMAPWorkers; i++ {
		go srv.downloadWorker(i)
	}

	logrus.Infof("[HTTP] 初始化: IMAP workers=%d, 缓存=%d块(≈%dMB), 等待超时=%v",
		MaxIMAPWorkers, n, int64(n)*FileBlockSize/(1024*1024), BlockWaitTimeout)
	return srv
}

func (s *HTTPIMAPServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/httptoimap", s.handleStream)

	logrus.Infof("HTTP-to-IMAP server listening on %s", s.addr)
	return http.ListenAndServe(s.addr, mux)
}

func (s *HTTPIMAPServer) StartAsync() {
	go func() {
		if err := s.Start(); err != nil {
			logrus.Errorf("HTTP server error: %v", err)
		}
	}()
}

// ──────────────────────────────────────────────────────────────────────────────
// downloadWorker — 后台 IMAP 下载 worker
// ──────────────────────────────────────────────────────────────────────────────

func (s *HTTPIMAPServer) downloadWorker(id int) {
	logrus.Infof("[Worker %d] 启动", id)
	for task := range s.taskQueue {
		s.executeTask(id, task)
	}
}

func (s *HTTPIMAPServer) executeTask(workerID int, task *downloadTask) {
	key := blockCacheKey(task.imapDir, task.uid)

	// 再查一次缓存（可能在排队期间被别的 worker 填充了）
	if _, _, hit := s.blockCache.Get(task.imapDir, task.uid); hit {
		logrus.Debugf("[Worker %d] 排队期间缓存已命中: %s", workerID, key)
		s.finishWaiter(key, nil)
		return
	}

	// 检查是否还有活跃的等待者，全部断开则跳过
	if s.allWaitersCancelled(key) {
		logrus.Debugf("[Worker %d] 所有等待者已断开，跳过: %s", workerID, key)
		s.finishWaiter(key, context.Canceled)
		return
	}

	logrus.Debugf("[Worker %d] 开始下载: %s", workerID, key)

	mt, data, err := downloadUIDForHTTP(task.imapDir, task.uid)
	if err != nil {
		// 下载失败，但如果所有等待者都已断开，只是静默丢弃
		if s.allWaitersCancelled(key) {
			logrus.Debugf("[Worker %d] 下载出错但所有等待者已断开: %s", workerID, key)
			s.finishWaiter(key, context.Canceled)
			return
		}
		logrus.Errorf("[Worker %d] 下载失败: %s — %v", workerID, key, err)
		s.finishWaiter(key, err)
		return
	}

	// 无论等待者是否还在，数据拿到了就写入缓存（下次可命中）
	s.blockCache.Put(task.imapDir, task.uid, mt, data)
	logrus.Debugf("[Worker %d] 下载完成并缓存: %s, %d bytes", workerID, key, len(data))
	s.finishWaiter(key, nil)
}

// finishWaiter 通知所有等待该块的请求
func (s *HTTPIMAPServer) finishWaiter(key string, err error) {
	s.waiterMu.Lock()
	w, ok := s.waiters[key]
	if ok {
		w.err = err
		close(w.done)
		delete(s.waiters, key)
	}
	s.waiterMu.Unlock()
}

// allWaitersCancelled 检查某个块的所有等待者是否都已取消
func (s *HTTPIMAPServer) allWaitersCancelled(key string) bool {
	s.waiterMu.Lock()
	defer s.waiterMu.Unlock()
	w, ok := s.waiters[key]
	if !ok {
		return true
	}
	for _, c := range w.refCtxs {
		if c.Err() == nil {
			return false
		}
	}
	return true
}

// ──────────────────────────────────────────────────────────────────────────────
// fetchBlock — HTTP 请求调用的块获取入口
//
// 1. 查缓存 → 命中直接返回
// 2. 未命中 → 提交下载任务（去重）→ 等待完成或超时
// 3. 完成后再从缓存取数据返回
// ──────────────────────────────────────────────────────────────────────────────

func (s *HTTPIMAPServer) fetchBlock(ctx context.Context, imapDir string, uid int64) (*MailText, []byte, error) {
	// 总超时：从首次调用开始计算
	deadline := time.NewTimer(BlockWaitTimeout)
	defer deadline.Stop()

	for {
		// 1. 查缓存
		if mt, data, hit := s.blockCache.Get(imapDir, uid); hit {
			return mt, data, nil
		}

		// 2. 请求已断开
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("request cancelled: %w", ctx.Err())
		}

		// 3. 提交下载任务并获取等待通道
		done, err := s.submitDownload(ctx, imapDir, uid)
		if err != nil {
			// submitDownload 返回错误只在入队时 ctx 已断开
			return nil, nil, err
		}

		// 4. 等待：缓存被填充 / 超时 / 请求断开
		select {
		case <-done:
			// 任务完成 → 检查缓存
			if mt, data, hit := s.blockCache.Get(imapDir, uid); hit {
				return mt, data, nil
			}
			// 缓存中没数据（任务被跳过或失败），循环重新提交
			logrus.Debugf("[fetchBlock] done 但缓存未命中 uid=%d，重新提交", uid)
			continue
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("request cancelled while waiting: %w", ctx.Err())
		case <-deadline.C:
			return nil, nil, fmt.Errorf("timeout waiting for block uid=%d after %v", uid, BlockWaitTimeout)
		}
	}
}

// submitDownload 提交下载任务，返回等待通道。
// 对同一个块做去重：如果已有任务在排队/下载中，直接复用其 done channel。
func (s *HTTPIMAPServer) submitDownload(ctx context.Context, imapDir string, uid int64) (chan struct{}, error) {
	key := blockCacheKey(imapDir, uid)

	s.waiterMu.Lock()

	// 已有等待器 → 去重，加入等待者列表
	if w, ok := s.waiters[key]; ok {
		w.refCtxs = append(w.refCtxs, ctx)
		done := w.done
		s.waiterMu.Unlock()
		logrus.Debugf("[Scheduler] 复用已有下载任务: %s", key)
		return done, nil
	}

	// 新建等待器
	w := &blockWaiter{
		done:    make(chan struct{}),
		refCtxs: []context.Context{ctx},
	}
	s.waiters[key] = w
	s.waiterMu.Unlock()

	// 提交任务到队列（task 不绑定单个请求的 ctx）
	task := &downloadTask{
		imapDir: imapDir,
		uid:     uid,
	}

	select {
	case s.taskQueue <- task:
		logrus.Debugf("[Scheduler] 任务入队: %s", key)
	case <-ctx.Done():
		// 当前请求断开了，但可能有其他等待者（去重场景）
		// 不清理 waiter，让 worker 通过 allWaitersCancelled 判断
		return nil, fmt.Errorf("request cancelled before task enqueued: %w", ctx.Err())
	}

	return w.done, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP 处理 — 不限并发，所有请求查缓存等待
// ──────────────────────────────────────────────────────────────────────────────

func (s *HTTPIMAPServer) handleStream(w http.ResponseWriter, r *http.Request) {
	imapDirB64 := r.URL.Query().Get("imapdir")
	localPathB64 := r.URL.Query().Get("localpath")

	if imapDirB64 == "" || localPathB64 == "" {
		http.Error(w, "missing imapdir or localpath parameter", http.StatusBadRequest)
		return
	}

	imapDir, err := decodeBase64Param(imapDirB64)
	if err != nil {
		http.Error(w, "invalid base64 for imapdir", http.StatusBadRequest)
		return
	}

	localPath, err := decodeBase64Param(localPathB64)
	if err != nil {
		http.Error(w, "invalid base64 for localpath", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	logrus.Infof("[HTTP] 请求: imapdir=%s, localpath=%s, Range=%s",
		imapDir, localPath, r.Header.Get("Range"))

	// ── 查找文件缓存记录 ─────────────────────────────
	files, err := getCacheFileFromDB(imapDir, localPath)
	if err != nil || len(files) == 0 {
		logrus.Errorf("[HTTP] 文件未找到: %v", err)
		http.Error(w, "file not found in cache", http.StatusNotFound)
		return
	}

	cf := files[0]
	blocks, err := getCacheBlockFromDB(cf.FileID)
	if err != nil {
		http.Error(w, "failed to get block info", http.StatusInternalServerError)
		return
	}
	cf.Blocks = blocks

	if int64(len(blocks)) != cf.BlockCount {
		http.Error(w, "incomplete file cache", http.StatusInternalServerError)
		return
	}

	sortBlocks(cf.Blocks)

	// ── 获取第一块确定文件大小 ──────────────────────
	firstBlock := cf.Blocks[0]
	mailText, firstBlockData, err := s.fetchBlock(ctx, imapDir, firstBlock.UID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		logrus.Errorf("[HTTP] 首块获取失败: %v", err)
		http.Error(w, "failed to get first block", http.StatusInternalServerError)
		return
	}

	totalSize := mailText.Vfilesize
	if totalSize <= 0 {
		totalSize = (cf.BlockCount-1)*FileBlockSize + int64(len(firstBlockData))
	}

	// ── 解析 Range ──────────────────────────────────
	rangeHeader := r.Header.Get("Range")
	var rangeStart, rangeEnd int64
	isRange := false

	if rangeHeader != "" {
		rangeStart, rangeEnd, isRange = parseRange(rangeHeader, totalSize)
		if !isRange {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", totalSize))
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	} else {
		rangeStart = 0
		rangeEnd = totalSize - 1
	}

	contentLen := rangeEnd - rangeStart + 1

	// ── 响应头 ──────────────────────────────────────
	w.Header().Set("Content-Type", guessContentType(localPath))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(contentLen, 10))

	if isRange {
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", rangeStart, rangeEnd, totalSize))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	// ── 逐块写入响应 ────────────────────────────────
	startBlockIdx := rangeStart / FileBlockSize
	startOffsetInBlock := rangeStart % FileBlockSize
	bytesRemaining := contentLen
	flusher, canFlush := w.(http.Flusher)

	for blockIdx := startBlockIdx; bytesRemaining > 0; blockIdx++ {
		if ctx.Err() != nil {
			logrus.Infof("[HTTP] 客户端断开（块 %d）", blockIdx+1)
			return
		}

		if int(blockIdx) >= len(cf.Blocks) {
			break
		}

		var blockData []byte

		if blockIdx == 0 && firstBlockData != nil {
			blockData = firstBlockData
			firstBlockData = nil
		} else {
			block := cf.Blocks[blockIdx]
			_, blockData, err = s.fetchBlock(ctx, imapDir, block.UID)
			if err != nil {
				if ctx.Err() != nil {
					logrus.Infof("[HTTP] 客户端断开（块 %d 等待中）", blockIdx+1)
				} else {
					logrus.Errorf("[HTTP] 块 %d 获取失败: %v", blockIdx+1, err)
				}
				return
			}
		}

		offsetInBlock := int64(0)
		if blockIdx == startBlockIdx {
			offsetInBlock = startOffsetInBlock
		}

		available := int64(len(blockData)) - offsetInBlock
		if available <= 0 {
			break
		}

		toWrite := available
		if toWrite > bytesRemaining {
			toWrite = bytesRemaining
		}

		n, err := w.Write(blockData[offsetInBlock : offsetInBlock+toWrite])
		if err != nil {
			logrus.Infof("[HTTP] 写入中断: %v", err)
			return
		}

		bytesRemaining -= int64(n)
		if canFlush {
			flusher.Flush()
		}
	}

	cacheCount, cacheMB := s.blockCache.Stats()
	logrus.Infof("[HTTP] 完成: %s, %d-%d, 缓存: %d块/%dMB",
		lastSegment(localPath), rangeStart, rangeEnd, cacheCount, cacheMB)
}

// ──────────────────────────────────────────────────────────────────────────────
// IMAP 连接管理
// ──────────────────────────────────────────────────────────────────────────────

var (
	httpFS   *MailFileSystem
	httpFSMu sync.Mutex
)

func getHTTPMailFS() (*MailFileSystem, error) {
	httpFSMu.Lock()
	defer httpFSMu.Unlock()

	if httpFS != nil && httpFS.c != nil {
		return httpFS, nil
	}

	httpFS = NewMailFileSystem()
	if httpFS.c == nil {
		return nil, fmt.Errorf("HTTP IMAP login failed")
	}
	return httpFS, nil
}

func downloadUIDForHTTP(imapDir string, uid int64) (*MailText, []byte, error) {
	fs, err := getHTTPMailFS()
	if err != nil {
		return nil, nil, err
	}

	httpFSMu.Lock()
	if fs.remoteDir != imapDir {
		if err := fs.Enter(imapDir); err != nil {
			httpFSMu.Unlock()
			resetHTTPFS()
			return nil, nil, fmt.Errorf("enter dir failed: %w", err)
		}
	}
	httpFSMu.Unlock()

	mailText, data, err := fs.downloadUID(uid)
	if err != nil {
		resetHTTPFS()
		return nil, nil, err
	}
	return mailText, data, nil
}

func resetHTTPFS() {
	httpFSMu.Lock()
	defer httpFSMu.Unlock()
	if httpFS != nil {
		httpFS.Logout()
		httpFS = nil
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Range 解析
// ──────────────────────────────────────────────────────────────────────────────

func parseRange(header string, totalSize int64) (int64, int64, bool) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if idx := strings.Index(spec, ","); idx >= 0 {
		spec = spec[:idx]
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}

	startStr := strings.TrimSpace(parts[0])
	endStr := strings.TrimSpace(parts[1])
	var start, end int64

	if startStr == "" {
		suffix, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		start = totalSize - suffix
		if start < 0 {
			start = 0
		}
		end = totalSize - 1
	} else {
		var err error
		start, err = strconv.ParseInt(startStr, 10, 64)
		if err != nil || start < 0 {
			return 0, 0, false
		}
		if endStr == "" {
			end = totalSize - 1
		} else {
			end, err = strconv.ParseInt(endStr, 10, 64)
			if err != nil {
				return 0, 0, false
			}
		}
	}

	if start > end || start >= totalSize {
		return 0, 0, false
	}
	if end >= totalSize {
		end = totalSize - 1
	}
	return start, end, true
}

// ──────────────────────────────────────────────────────────────────────────────
// 辅助函数
// ──────────────────────────────────────────────────────────────────────────────

func lastSegment(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimRight(path, "/")
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return path
	}
	return path[idx+1:]
}

func decodeBase64Param(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(s)
			if err != nil {
				b, err = base64.RawURLEncoding.DecodeString(s)
			}
		}
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func sortBlocks(blocks []CacheBlock) {
	for i := 1; i < len(blocks); i++ {
		for j := i; j > 0 && blocks[j].BlockSeq < blocks[j-1].BlockSeq; j-- {
			blocks[j], blocks[j-1] = blocks[j-1], blocks[j]
		}
	}
}

func guessContentType(path string) string {
	path = strings.ToLower(path)
	switch {
	case strings.HasSuffix(path, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(path, ".mkv"):
		return "video/x-matroska"
	case strings.HasSuffix(path, ".avi"):
		return "video/x-msvideo"
	case strings.HasSuffix(path, ".mov"):
		return "video/quicktime"
	case strings.HasSuffix(path, ".wmv"):
		return "video/x-ms-wmv"
	case strings.HasSuffix(path, ".flv"):
		return "video/x-flv"
	case strings.HasSuffix(path, ".webm"):
		return "video/webm"
	case strings.HasSuffix(path, ".ts"):
		return "video/mp2t"
	case strings.HasSuffix(path, ".m3u8"):
		return "application/vnd.apple.mpegurl"
	case strings.HasSuffix(path, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(path, ".flac"):
		return "audio/flac"
	case strings.HasSuffix(path, ".wav"):
		return "audio/wav"
	case strings.HasSuffix(path, ".aac"):
		return "audio/aac"
	case strings.HasSuffix(path, ".ogg"):
		return "audio/ogg"
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".gif"):
		return "image/gif"
	case strings.HasSuffix(path, ".webp"):
		return "image/webp"
	case strings.HasSuffix(path, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(path, ".zip"):
		return "application/zip"
	case strings.HasSuffix(path, ".rar"):
		return "application/x-rar-compressed"
	case strings.HasSuffix(path, ".7z"):
		return "application/x-7z-compressed"
	case strings.HasSuffix(path, ".txt"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
