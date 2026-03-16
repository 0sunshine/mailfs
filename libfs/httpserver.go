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
// ──────────────────────────────────────────────────────────────────────────────

const (
	DefaultBlockCacheSize = 32
	MaxIMAPWorkers        = 3
	BlockWaitTimeout      = 60 * time.Second
)

type downloadTask struct {
	imapDir string
	uid     int64
}

type blockWaiter struct {
	done    chan struct{}
	err     error
	refCtxs []context.Context
}

type HTTPIMAPServer struct {
	addr       string
	blockCache *BlockLRUCache

	taskQueue chan *downloadTask

	waiterMu sync.Mutex
	waiters  map[string]*blockWaiter
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

	for i := 0; i < MaxIMAPWorkers; i++ {
		go srv.downloadWorker(i)
	}

	logrus.Infof("[HTTP] 初始化: IMAP workers=%d, 缓存=%d块, 等待超时=%v",
		MaxIMAPWorkers, n, BlockWaitTimeout)
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
// downloadWorker
// ──────────────────────────────────────────────────────────────────────────────

func (s *HTTPIMAPServer) downloadWorker(id int) {
	logrus.Infof("[Worker %d] 启动", id)
	for task := range s.taskQueue {
		s.executeTask(id, task)
	}
}

func (s *HTTPIMAPServer) executeTask(workerID int, task *downloadTask) {
	key := blockCacheKey(task.imapDir, task.uid)

	if _, _, hit := s.blockCache.Get(task.imapDir, task.uid); hit {
		logrus.Debugf("[Worker %d] 排队期间缓存已命中: %s", workerID, key)
		s.finishWaiter(key, nil)
		return
	}

	if s.allWaitersCancelled(key) {
		logrus.Debugf("[Worker %d] 所有等待者已断开，跳过: %s", workerID, key)
		s.finishWaiter(key, context.Canceled)
		return
	}

	logrus.Debugf("[Worker %d] 开始下载: %s", workerID, key)

	mt, data, err := downloadUIDForHTTP(task.imapDir, task.uid)
	if err != nil {
		if s.allWaitersCancelled(key) {
			logrus.Debugf("[Worker %d] 下载出错但所有等待者已断开: %s", workerID, key)
			s.finishWaiter(key, context.Canceled)
			return
		}
		logrus.Errorf("[Worker %d] 下载失败: %s — %v", workerID, key, err)
		s.finishWaiter(key, err)
		return
	}

	s.blockCache.Put(task.imapDir, task.uid, mt, data)
	logrus.Debugf("[Worker %d] 下载完成并缓存: %s, %d bytes", workerID, key, len(data))
	s.finishWaiter(key, nil)
}

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
// fetchBlock
// ──────────────────────────────────────────────────────────────────────────────

func (s *HTTPIMAPServer) fetchBlock(ctx context.Context, imapDir string, uid int64) (*MailText, []byte, error) {
	deadline := time.NewTimer(BlockWaitTimeout)
	defer deadline.Stop()

	for {
		if mt, data, hit := s.blockCache.Get(imapDir, uid); hit {
			return mt, data, nil
		}

		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("request cancelled: %w", ctx.Err())
		}

		done, err := s.submitDownload(ctx, imapDir, uid)
		if err != nil {
			return nil, nil, err
		}

		select {
		case <-done:
			if mt, data, hit := s.blockCache.Get(imapDir, uid); hit {
				return mt, data, nil
			}
			logrus.Debugf("[fetchBlock] done 但缓存未命中 uid=%d，重新提交", uid)
			continue
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("request cancelled while waiting: %w", ctx.Err())
		case <-deadline.C:
			return nil, nil, fmt.Errorf("timeout waiting for block uid=%d after %v", uid, BlockWaitTimeout)
		}
	}
}

func (s *HTTPIMAPServer) submitDownload(ctx context.Context, imapDir string, uid int64) (chan struct{}, error) {
	key := blockCacheKey(imapDir, uid)

	s.waiterMu.Lock()

	if w, ok := s.waiters[key]; ok {
		w.refCtxs = append(w.refCtxs, ctx)
		done := w.done
		s.waiterMu.Unlock()
		logrus.Debugf("[Scheduler] 复用已有下载任务: %s", key)
		return done, nil
	}

	w := &blockWaiter{
		done:    make(chan struct{}),
		refCtxs: []context.Context{ctx},
	}
	s.waiters[key] = w
	s.waiterMu.Unlock()

	task := &downloadTask{
		imapDir: imapDir,
		uid:     uid,
	}

	select {
	case s.taskQueue <- task:
		logrus.Debugf("[Scheduler] 任务入队: %s", key)
	case <-ctx.Done():
		return nil, fmt.Errorf("request cancelled before task enqueued: %w", ctx.Err())
	}

	return w.done, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// blockOffsetTable — 根据数据库中每块的 blocksize 构建累积偏移表
//
// 返回 offsets 长度 = len(blocks)+1，offsets[i] 是第 i 块的起始字节偏移，
// offsets[len(blocks)] 是文件总大小。
// 如果数据库中的 blocksize 为 0（旧数据），则回退使用 DefaultFileBlockSize。
// ──────────────────────────────────────────────────────────────────────────────

func buildBlockOffsets(blocks []CacheBlock, fileSize int64) []int64 {
	n := len(blocks)
	offsets := make([]int64, n+1)
	offsets[0] = 0

	allZero := true
	for _, b := range blocks {
		if b.BlockSize > 0 {
			allZero = false
			break
		}
	}

	if allZero && fileSize > 0 {
		// 旧数据：blocksize 全为 0，回退用 DefaultFileBlockSize 估算
		for i := 0; i < n; i++ {
			if i < n-1 {
				offsets[i+1] = offsets[i] + DefaultFileBlockSize
			} else {
				offsets[i+1] = fileSize
			}
		}
		return offsets
	}

	for i, b := range blocks {
		bs := b.BlockSize
		if bs <= 0 {
			// 单块缺失 blocksize，用默认值
			bs = DefaultFileBlockSize
		}
		offsets[i+1] = offsets[i] + bs
	}

	// 如果有 fileSize 且比累积偏移更可靠，用 fileSize 修正最后一块
	if fileSize > 0 && offsets[n] != fileSize {
		offsets[n] = fileSize
	}

	return offsets
}

// findBlockForOffset 在 offsets 表中找到给定字节偏移所在的块索引和块内偏移
func findBlockForOffset(offsets []int64, byteOffset int64) (blockIdx int, offsetInBlock int64) {
	n := len(offsets) - 1 // 块数
	for i := 0; i < n; i++ {
		if byteOffset < offsets[i+1] {
			return i, byteOffset - offsets[i]
		}
	}
	// 落在最后一块
	if n > 0 {
		return n - 1, byteOffset - offsets[n-1]
	}
	return 0, byteOffset
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP 处理
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

	// ── 确定文件总大小 ──────────────────────────────
	// 优先使用数据库中记录的 filesize
	totalSize := cf.FileSize

	if totalSize <= 0 {
		// 数据库无 filesize（旧数据），需 fetch 首块来获取
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

		totalSize = mailText.Vfilesize
		if totalSize <= 0 {
			// 最后手段：用块偏移表估算
			offsets := buildBlockOffsets(cf.Blocks, 0)
			// 修正最后一块：用实际首块数据长度
			if len(cf.Blocks) == 1 {
				totalSize = int64(len(firstBlockData))
			} else {
				totalSize = offsets[len(cf.Blocks)]
			}
		}

		// 将 firstBlockData 写入缓存以避免重复 fetch
		_ = firstBlockData // 首块已在 LRU 缓存中（fetchBlock 内部会 Put）
	}

	// ── 构建块偏移表 ────────────────────────────────
	offsets := buildBlockOffsets(cf.Blocks, totalSize)

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

	// ── 逐块写入响应（使用偏移表定位）────────────────
	startBlockIdx, startOffsetInBlock := findBlockForOffset(offsets, rangeStart)
	bytesRemaining := contentLen
	flusher, canFlush := w.(http.Flusher)

	for blockIdx := startBlockIdx; bytesRemaining > 0; blockIdx++ {
		if ctx.Err() != nil {
			logrus.Infof("[HTTP] 客户端断开（块 %d）", blockIdx+1)
			return
		}

		if blockIdx >= len(cf.Blocks) {
			break
		}

		block := cf.Blocks[blockIdx]
		_, blockData, err := s.fetchBlock(ctx, imapDir, block.UID)
		if err != nil {
			if ctx.Err() != nil {
				logrus.Infof("[HTTP] 客户端断开（块 %d 等待中）", blockIdx+1)
			} else {
				logrus.Errorf("[HTTP] 块 %d 获取失败: %v", blockIdx+1, err)
			}
			return
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
