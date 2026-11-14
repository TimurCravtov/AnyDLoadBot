package bot

import "sync"

const maxCachedFiles = 1000

// fileCache remembers the Telegram file_id of videos already uploaded to the
// storage chat, so repeating an inline query for the same link is instant.
// When full it is simply emptied; it only saves work, never correctness.
type fileCache struct {
	mu  sync.Mutex
	max int
	m   map[string]cachedFile
}

type cachedFile struct {
	fileID string
	title  string
}

func newFileCache(max int) *fileCache {
	return &fileCache{max: max, m: make(map[string]cachedFile)}
}

func (c *fileCache) get(link string) (cachedFile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.m[link]
	return f, ok
}

func (c *fileCache) put(link string, f cachedFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		clear(c.m)
	}
	c.m[link] = f
}
