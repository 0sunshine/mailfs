package libfs

import (
	"container/list"
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"
)

// ──────────────────────────────────────────────────────────────────────────────
// BlockLRUCache — 基于 LRU 策略的邮件块内存缓存
//
// key = "imapDir|uid"，同一 UID 在不同邮箱目录下不会冲突。
// 每个块最大 32MB (FileBlockSize)，默认缓存 8 个块 ≈ 256MB 内存峰值。
// 线程安全，可供多个 goroutine 并发读写。
// ──────────────────────────────────────────────────────────────────────────────

// blockCacheKey 生成缓存 key: "imapDir|uid"
func blockCacheKey(imapDir string, uid int64) string {
	return fmt.Sprintf("%s|%d", imapDir, uid)
}

// blockCacheEntry 缓存条目
type blockCacheEntry struct {
	key      string
	mailText *MailText
	data     []byte
}

// BlockLRUCache LRU 块缓存
type BlockLRUCache struct {
	mu       sync.Mutex
	capacity int                        // 最大缓存块数
	ll       *list.List                 // 双向链表，前端 = 最近使用
	items    map[string]*list.Element   // key → list element
	memBytes int64                      // 当前缓存占用的总字节数（仅 data 部分）
}

// NewBlockLRUCache 创建一个新的 LRU 缓存，capacity 为最大缓存块数
func NewBlockLRUCache(capacity int) *BlockLRUCache {
	if capacity <= 0 {
		capacity = 8
	}
	return &BlockLRUCache{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[string]*list.Element, capacity),
	}
}

// Get 从缓存获取块数据。命中时将条目移到链表前端（最近使用）。
// 返回 (mailText, data, hit)
func (c *BlockLRUCache) Get(imapDir string, uid int64) (*MailText, []byte, bool) {
	key := blockCacheKey(imapDir, uid)

	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return nil, nil, false
	}

	// 移到前端（最近使用）
	c.ll.MoveToFront(elem)
	entry := elem.Value.(*blockCacheEntry)

	logrus.Debugf("[LRU] 缓存命中: key=%s, dataLen=%d, 缓存块数=%d, 内存≈%dMB",
		key, len(entry.data), c.ll.Len(), c.memBytes/(1024*1024))

	return entry.mailText, entry.data, true
}

// Put 将块数据放入缓存。如果 key 已存在则更新并移到前端；
// 如果缓存已满则淘汰最久未使用的条目（链表尾部）。
func (c *BlockLRUCache) Put(imapDir string, uid int64, mailText *MailText, data []byte) {
	key := blockCacheKey(imapDir, uid)

	c.mu.Lock()
	defer c.mu.Unlock()

	// 如果已存在，更新数据并移到前端
	if elem, ok := c.items[key]; ok {
		c.ll.MoveToFront(elem)
		old := elem.Value.(*blockCacheEntry)
		c.memBytes -= int64(len(old.data))
		old.mailText = mailText
		old.data = data
		c.memBytes += int64(len(data))

		logrus.Debugf("[LRU] 缓存更新: key=%s, dataLen=%d", key, len(data))
		return
	}

	// 缓存已满，淘汰尾部（最久未使用）
	for c.ll.Len() >= c.capacity {
		c.evictOldest()
	}

	// 插入新条目到前端
	entry := &blockCacheEntry{
		key:      key,
		mailText: mailText,
		data:     data,
	}
	elem := c.ll.PushFront(entry)
	c.items[key] = elem
	c.memBytes += int64(len(data))

	logrus.Debugf("[LRU] 缓存写入: key=%s, dataLen=%d, 缓存块数=%d, 内存≈%dMB",
		key, len(data), c.ll.Len(), c.memBytes/(1024*1024))
}

// evictOldest 淘汰链表尾部的条目（最久未使用）。调用前必须持有 mu 锁。
func (c *BlockLRUCache) evictOldest() {
	tail := c.ll.Back()
	if tail == nil {
		return
	}

	entry := tail.Value.(*blockCacheEntry)

	logrus.Debugf("[LRU] 淘汰: key=%s, dataLen=%d, 释放≈%dMB",
		entry.key, len(entry.data), int64(len(entry.data))/(1024*1024))

	c.ll.Remove(tail)
	delete(c.items, entry.key)
	c.memBytes -= int64(len(entry.data))
}

// Len 返回当前缓存中的块数
func (c *BlockLRUCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// MemBytes 返回当前缓存占用的数据字节数（不含 MailText 等元信息）
func (c *BlockLRUCache) MemBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.memBytes
}

// Clear 清空所有缓存
func (c *BlockLRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ll.Init()
	c.items = make(map[string]*list.Element, c.capacity)
	c.memBytes = 0

	logrus.Infof("[LRU] 缓存已清空")
}

// Stats 返回缓存统计信息（用于日志/调试）
func (c *BlockLRUCache) Stats() (count int, memMB int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len(), c.memBytes / (1024 * 1024)
}
