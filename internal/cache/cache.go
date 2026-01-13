package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alecf/heyman/internal/config"
	"github.com/alecf/heyman/internal/llm"
)

// Entry represents a cached response
type Entry struct {
	Key        string              `json:"key"`
	Command    string              `json:"command"`
	Question   string              `json:"question"`
	Model      string              `json:"model"`
	Response   *llm.QueryResponse  `json:"response"`
	CreatedAt  time.Time           `json:"created_at"`
	AccessedAt time.Time           `json:"accessed_at"`
	AccessCount int                `json:"access_count"`
}

// Cache manages response caching
type Cache struct {
	cacheDir  string
	maxAgeDays int
}

// New creates a new cache manager
func New(maxAgeDays int) *Cache {
	return &Cache{
		cacheDir:   config.GetCacheDir(),
		maxAgeDays: maxAgeDays,
	}
}

// Get retrieves a cached response
func (c *Cache) Get(command, question, model string) (*llm.QueryResponse, bool) {
	key := GenerateKey(command, question, model)
	entryPath := filepath.Join(c.cacheDir, key+".json")

	// Check if cached file exists
	data, err := os.ReadFile(entryPath)
	if err != nil {
		return nil, false
	}

	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}

	// Check if entry has expired
	if c.isExpired(entry.CreatedAt) {
		// Delete expired entry
		os.Remove(entryPath)
		return nil, false
	}

	// Validate response is not nil (could be nil from corrupted cache)
	if entry.Response == nil {
		// Delete corrupted entry
		os.Remove(entryPath)
		return nil, false
	}

	// Update access metadata
	entry.AccessedAt = time.Now()
	entry.AccessCount++
	c.saveEntry(&entry)

	// Mark response as cached
	response := entry.Response
	response.Cached = true

	return response, true
}

// Set stores a response in the cache
func (c *Cache) Set(command, question, model string, response *llm.QueryResponse) error {
	key := GenerateKey(command, question, model)

	entry := &Entry{
		Key:         key,
		Command:     command,
		Question:    question,
		Model:       model,
		Response:    response,
		CreatedAt:   time.Now(),
		AccessedAt:  time.Now(),
		AccessCount: 1,
	}

	return c.saveEntry(entry)
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

	// Write cache file with restricted permissions (0600 for security)
	if err := os.WriteFile(entryPath, data, 0600); err != nil {
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
			if err := json.Unmarshal(data, &cacheEntry); err != nil {
				continue
			}

			if c.isExpired(cacheEntry.CreatedAt) {
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
	TotalEntries int       `json:"total_entries"`
	TotalSizeBytes int64   `json:"total_size_bytes"`
	OldestEntry  *time.Time `json:"oldest_entry,omitempty"`
	NewestEntry  *time.Time `json:"newest_entry,omitempty"`
	TotalHits    int        `json:"total_hits"`
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

		stats.TotalEntries++
		stats.TotalSizeBytes += info.Size()

		// Read entry for detailed stats
		data, err := os.ReadFile(entryPath)
		if err != nil {
			continue
		}

		var cacheEntry Entry
		if err := json.Unmarshal(data, &cacheEntry); err != nil {
			continue
		}

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
