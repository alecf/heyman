package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type val struct {
	Command string `json:"command"`
}

func TestSetGet(t *testing.T) {
	c := NewAt(t.TempDir(), 30)
	key := GenerateKey("m", "q")
	var v val
	if c.Get(key, &v) {
		t.Fatal("hit on empty cache")
	}
	if err := c.Set(key, "ls", "q", "m", val{"ls -la"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		v = val{}
		if !c.Get(key, &v) || v.Command != "ls -la" {
			t.Fatalf("get %d = %+v", i, v)
		}
	}
	st, err := c.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalEntries != 1 || st.TotalHits != 3 || st.OldestEntry == nil {
		t.Errorf("stats = %+v", st)
	}
	info, err := os.Stat(filepath.Join(c.Dir(), key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("entry perm = %o", perm)
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(c.Dir())
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestGenerateKey(t *testing.T) {
	a := GenerateKey("m", "", "command", "ls", "", "q", "darwin")
	if a != GenerateKey("m", "", "command", "ls", "", "q", "darwin") {
		t.Error("not deterministic")
	}
	for _, other := range [][]string{
		{"m", "http://other", "command", "ls", "", "q", "darwin"},
		{"m", "", "explain", "ls", "", "q", "darwin"},
		{"m", "", "command", "ls", "1", "q", "darwin"},
		{"m", "", "command", "ls", "", "q", "linux"},
		{"m", "", "command", "l", "s", "q", "darwin"}, // separator matters
	} {
		if GenerateKey(other...) == a {
			t.Errorf("key collision for %q", other)
		}
	}
	if !validKey(a) {
		t.Error("generated key not valid")
	}
}

func TestInvalidKeys(t *testing.T) {
	c := NewAt(t.TempDir(), 30)
	for _, k := range []string{"", "../x", "a/b", "ABC", strings.Repeat("a", 200)} {
		if err := c.Set(k, "", "", "", val{"x"}); err == nil {
			t.Errorf("Set(%q) accepted", k)
		}
		var v val
		if c.Get(k, &v) {
			t.Errorf("Get(%q) hit", k)
		}
	}
}

func writeEntry(t *testing.T, c *Cache, key string, created time.Time) {
	t.Helper()
	raw, _ := json.Marshal(val{"x"})
	e := &Entry{Key: key, Response: raw, CreatedAt: created, AccessedAt: created}
	if err := c.saveEntry(e); err != nil {
		t.Fatal(err)
	}
}

func TestExpiry(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	key := GenerateKey("old")

	c := NewAt(dir, 1)
	writeEntry(t, c, key, old)
	var v val
	if c.Get(key, &v) {
		t.Error("expired entry served")
	}
	if _, err := os.Stat(filepath.Join(dir, key+".json")); !os.IsNotExist(err) {
		t.Error("expired entry not removed")
	}

	// CacheDays <= 0 means never expire.
	for _, days := range []int{0, -1} {
		c := NewAt(dir, days)
		writeEntry(t, c, key, old.Add(-365*24*time.Hour))
		if !c.Get(key, &v) {
			t.Errorf("days=%d: entry expired", days)
		}
	}
}

func TestCleanExpiredAndClear(t *testing.T) {
	dir := t.TempDir()
	c := NewAt(dir, 1)
	writeEntry(t, c, GenerateKey("a"), time.Now().Add(-72*time.Hour))
	writeEntry(t, c, GenerateKey("b"), time.Now())
	if err := os.WriteFile(filepath.Join(dir, GenerateKey("corrupt")+".json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := c.CleanExpired()
	if err != nil || n != 2 {
		t.Errorf("CleanExpired = %d, %v; want 2 (expired + corrupt)", n, err)
	}
	n, err = c.Clear()
	if err != nil || n != 1 {
		t.Errorf("Clear = %d, %v", n, err)
	}
	// Missing directory is fine.
	missing := NewAt(filepath.Join(dir, "nope"), 1)
	if _, err := missing.CleanExpired(); err != nil {
		t.Error(err)
	}
	if st, err := missing.GetStats(); err != nil || st.TotalEntries != 0 {
		t.Error(st, err)
	}
}

func TestCorruptEntries(t *testing.T) {
	dir := t.TempDir()
	c := NewAt(dir, 30)
	for i, body := range []string{"", "{nope", `{"response":null}`, `{"response":"not an object","created_at":"2999-01-01T00:00:00Z"}`} {
		key := GenerateKey("c", string(rune('a'+i)))
		path := filepath.Join(dir, key+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		var v val
		if c.Get(key, &v) {
			t.Errorf("corrupt entry %q served", body)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("corrupt entry %q not removed", body)
		}
	}
}

func TestSetSweepsExpired(t *testing.T) {
	dir := t.TempDir()
	c := NewAt(dir, 1)
	oldKey := GenerateKey("old")
	writeEntry(t, c, oldKey, time.Now().Add(-72*time.Hour))
	if err := c.Set(GenerateKey("new"), "", "", "", val{"x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, oldKey+".json")); !os.IsNotExist(err) {
		t.Error("Set didn't sweep expired entries")
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := NewAt(t.TempDir(), 30)
	key := GenerateKey("k")
	if err := c.Set(key, "", "", "", val{"x"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				var v val
				if !c.Get(key, &v) || v.Command != "x" {
					t.Error("miss or torn read during concurrent access")
					return
				}
				_ = c.Set(key, "", "", "", val{"x"})
			}
		}()
	}
	wg.Wait()
}

func TestNewUsesEnvDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HEYMAN_CACHE_DIR", dir)
	if New(30).Dir() != dir {
		t.Error("HEYMAN_CACHE_DIR ignored")
	}
}
