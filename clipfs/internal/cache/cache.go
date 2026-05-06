package cache

import (
	"container/list"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// DiskCache is an LRU disk-backed cache with a configurable max size in bytes.
type DiskCache struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	curBytes int64

	// LRU tracking: front = most recently used
	order *list.List
	items map[string]*list.Element
}

type entry struct {
	key  string
	size int64
}

// New creates a disk cache at dir with a maximum size of maxMB megabytes.
// If maxMB <= 0, the cache is unlimited.
func New(dir string, maxMB int) (*DiskCache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cache: cannot create dir %s: %w", dir, err)
	}

	// Clean any stale cache files from a previous run
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.Remove(filepath.Join(dir, e.Name()))
	}

	return &DiskCache{
		dir:      dir,
		maxBytes: int64(maxMB) * 1024 * 1024,
		order:    list.New(),
		items:    make(map[string]*list.Element),
	}, nil
}

func (c *DiskCache) keyToPath(key string) string {
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, fmt.Sprintf("%x.mkv", hash[:16]))
}

// Get returns the cached file path and size if it exists, or ("", 0, false).
func (c *DiskCache) Get(key string) (string, int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return "", 0, false
	}

	// Move to front (most recently used)
	c.order.MoveToFront(elem)
	e := elem.Value.(*entry)

	path := c.keyToPath(key)
	return path, e.size, true
}

// Put writes data to the cache under key, evicting LRU entries if needed.
func (c *DiskCache) Put(key string, data []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If already cached, remove old entry first
	if elem, ok := c.items[key]; ok {
		c.removeLocked(elem)
	}

	size := int64(len(data))

	// Evict until there's room (if maxBytes is set)
	if c.maxBytes > 0 {
		for c.curBytes+size > c.maxBytes && c.order.Len() > 0 {
			c.evictOldest()
		}
	}

	path := c.keyToPath(key)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("cache: write %s: %w", path, err)
	}

	e := &entry{key: key, size: size}
	elem := c.order.PushFront(e)
	c.items[key] = elem
	c.curBytes += size

	log.Printf("Cache: stored %s (%d MB, total cache: %d MB)", key, size/(1024*1024), c.curBytes/(1024*1024))

	return path, nil
}

// Size returns current cache size in bytes.
func (c *DiskCache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes
}

func (c *DiskCache) evictOldest() {
	elem := c.order.Back()
	if elem == nil {
		return
	}
	c.removeLocked(elem)
}

func (c *DiskCache) removeLocked(elem *list.Element) {
	e := elem.Value.(*entry)
	path := c.keyToPath(e.key)
	os.Remove(path)
	c.curBytes -= e.size
	c.order.Remove(elem)
	delete(c.items, e.key)
	log.Printf("Cache: evicted %s", e.key)
}
