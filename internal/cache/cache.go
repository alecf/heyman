package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alecf/heyman/internal/config"
)

// Entry represents a cached response
type Entry struct {
	Key         string          `json:"key"`
	Command     string          `json:"command"`
	Question    string          `json:"question"`
	Model       string          `json:"model"`
	Response    json.RawMessage `json:"response"`
	CreatedAt   time.Time       `json:"created_at"`
	AccessedAt  time.Time       `json:"accessed_at"`
	AccessCount int             `json:"access_count"`
}

// Cache manages response caching
type Cache struct {
	cacheDir   string
	maxAgeDays int
}

// New creates a cache manager in config.GetCacheDir(). Entries older than
// maxAgeDays expire; maxAgeDays <= 0 means entries never expire.
func New(maxAgeDays int) *Cache {
	return NewAt(config.GetCacheDir(), maxAgeDays)
}

// NewAt creates a cache manager rooted at dir.
func NewAt(dir string, maxAgeDays int) *Cache {
	return &Cache{cacheDir: dir, maxAgeDays: maxAgeDays}
}

// Dir returns the cache directory.
func (c *Cache) Dir() string { return c.cacheDir }

// validKey keeps keys to the hex digests GenerateKey produces, so a key can
// never escape the cache directory.
func validKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, r := range key {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Get loads the cached value for key into v. It returns false on a miss,
// an expired entry, or an entry that can't be decoded into v.
func (c *Cache) Get(key string, v any) bool {
	if !validKey(key) {
		return false
	}
	entryPath := filepath.Join(c.cacheDir, key+".json")

	data, err := os.ReadFile(entryPath)
	if err != nil {
		return false
	}

	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil || len(entry.Response) == 0 || string(entry.Response) == "null" {
		os.Remove(entryPath)
		return false
	}

	if c.isExpired(entry.CreatedAt) {
		os.Remove(entryPath)
		return false
	}

	if err := json.Unmarshal(entry.Response, v); err != nil {
		os.Remove(entryPath)
		return false
	}

	// Best-effort access bookkeeping for cache-stats; a failure here
	// shouldn't fail the read. saveEntry is atomic, so a concurrent reader
	// never sees a half-written file.
	entry.AccessedAt = time.Now()
	entry.AccessCount++
	_ = c.saveEntry(&entry)

	return true
}

// Set stores v under key. command, question and model are recorded for
// cache-stats and debugging only.
func (c *Cache) Set(key, command, question, model string, v any) error {
	if !validKey(key) {
		return fmt.Errorf("invalid cache key %q", key)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal cache value: %w", err)
	}
	now := time.Now()
	entry := &Entry{
		Key:         key,
		Command:     command,
		Question:    question,
		Model:       model,
		Response:    raw,
		CreatedAt:   now,
		AccessedAt:  now,
		AccessCount: 0,
	}

	if err := c.saveEntry(entry); err != nil {
		return err
	}
	c.maybeCleanExpired()
	return nil
}

// cleanMarker records when expired entries were last swept.
const cleanMarker = ".last-clean"

// maybeCleanExpired sweeps expired entries at most once a day so the cache
// directory doesn't grow forever.
func (c *Cache) maybeCleanExpired() {
	if c.maxAgeDays <= 0 {
		return
	}
	marker := filepath.Join(c.cacheDir, cleanMarker)
	if info, err := os.Stat(marker); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
		return
	}
	_ = os.WriteFile(marker, nil, 0600)
	now := time.Now()
	_ = os.Chtimes(marker, now, now)
	_, _ = c.CleanExpired()
}

// saveEntry writes an entry to disk
func (c *Cache) saveEntry(entry *Entry) error {
	// Ensure cache directory exists (0700 for security)
	if err := os.MkdirAll(c.cacheDir, 0700); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	entryPath := filepath.Join(c.cacheDir, entry.Key+".json")

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cache entry: %w", err)
	}

	// Write to a temp file and rename so readers (and concurrent heymans)
	// never see a partial entry. CreateTemp uses 0600.
	tmp, err := os.CreateTemp(c.cacheDir, ".entry-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to write cache entry: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write cache entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write cache entry: %w", err)
	}
	if err := os.Rename(tmp.Name(), entryPath); err != nil {
		return fmt.Errorf("failed to write cache entry: %w", err)
	}
	return nil
}

// isExpired checks if an entry has expired
func (c *Cache) isExpired(createdAt time.Time) bool {
	if c.maxAgeDays <= 0 {
		return false // No expiration
	}
	expiryTime := createdAt.Add(time.Duration(c.maxAgeDays) * 24 * time.Hour)
	return time.Now().After(expiryTime)
}

// CleanExpired removes all expired entries
func (c *Cache) CleanExpired() (int, error) {
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read cache directory: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			entryPath := filepath.Join(c.cacheDir, entry.Name())

			data, err := os.ReadFile(entryPath)
			if err != nil {
				continue
			}

			var cacheEntry Entry
			if err := json.Unmarshal(data, &cacheEntry); err != nil || c.isExpired(cacheEntry.CreatedAt) {
				if err := os.Remove(entryPath); err == nil {
					removed++
				}
			}
		}
	}

	return removed, nil
}

// Clear removes all cached entries
func (c *Cache) Clear() (int, error) {
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read cache directory: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			entryPath := filepath.Join(c.cacheDir, entry.Name())
			if err := os.Remove(entryPath); err == nil {
				removed++
			}
		}
	}

	return removed, nil
}

// Stats returns cache statistics
type Stats struct {
	TotalEntries   int        `json:"total_entries"`
	TotalSizeBytes int64      `json:"total_size_bytes"`
	OldestEntry    *time.Time `json:"oldest_entry,omitempty"`
	NewestEntry    *time.Time `json:"newest_entry,omitempty"`
	TotalHits      int        `json:"total_hits"`
}

// GetStats returns cache statistics
func (c *Cache) GetStats() (*Stats, error) {
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &Stats{}, nil
		}
		return nil, fmt.Errorf("failed to read cache directory: %w", err)
	}

	stats := &Stats{}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		entryPath := filepath.Join(c.cacheDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		data, err := os.ReadFile(entryPath)
		if err != nil {
			continue
		}
		var cacheEntry Entry
		if err := json.Unmarshal(data, &cacheEntry); err != nil {
			continue // corrupt; Get/CleanExpired remove these
		}

		stats.TotalEntries++
		stats.TotalSizeBytes += info.Size()

		stats.TotalHits += cacheEntry.AccessCount

		if stats.OldestEntry == nil || cacheEntry.CreatedAt.Before(*stats.OldestEntry) {
			stats.OldestEntry = &cacheEntry.CreatedAt
		}

		if stats.NewestEntry == nil || cacheEntry.CreatedAt.After(*stats.NewestEntry) {
			stats.NewestEntry = &cacheEntry.CreatedAt
		}
	}

	return stats, nil
}
