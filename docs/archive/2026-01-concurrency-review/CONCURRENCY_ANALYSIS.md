# Go Concurrency and Goroutine Issues Analysis - Heyman Project

**Analysis Date:** 2026-01-13
**Project:** github.com/alecf/heyman
**Go Modules Scanned:** 19 files across 11 packages

---

## Executive Summary

The heyman project has several concurrency-related issues ranging from **CRITICAL race conditions** to **HIGH severity goroutine leaks** and **MEDIUM severity channel handling problems**. These issues are concentrated in three main areas:

1. **Spinner component** - Race conditions and improper synchronization
2. **Query executor** - Channel handling issues with potential panics
3. **LLM providers** - Goroutine leaks and incomplete channel consumption

---

## Issues Found

### CRITICAL Issues

#### 1. Race Condition: Spinner active flag and message field
**File:** `/Users/alecf/projects/aiman/internal/spinner/spinner.go`
**Lines:** 18, 38, 57-58, 70, 73
**Severity:** CRITICAL
**Issue Type:** Race Condition (Data Race)

**Description:**
The `Spinner` struct has unsynchronized access to the `active` and `message` fields from multiple goroutines:

- **Write race:** `active` is written in `Start()` (line 38), `Stop()` (line 73), and `StopWithMessage()` (line 90) without synchronization
- **Write race:** `message` is written in `Update()` (line 58) without synchronization
- **Read race:** The background goroutine (line 39-53) reads `message` in line 49 without synchronization
- **Read race:** Multiple methods check `active` flag without synchronization (lines 59, 70, 84)

**Example Problematic Code:**
```go
// Line 38 - Write without lock
s.active = true
go func() {
    // ...
    case <-s.done:
        return
    // ...
    fmt.Fprintf(s.writer, "\r%s %s", s.frames[frame], s.message)  // LINE 49 - Read
}

// Line 57-58 - Write without lock from different goroutine
func (s *Spinner) Update(message string) {
    s.message = message  // RACE: No synchronization
```

**Impact:**
- Memory corruption or inconsistent reads
- Undefined behavior when spinner is updated while animation runs
- Can cause crashes or corrupted output

**Recommended Fix:**
Add `sync.RWMutex` to protect concurrent access:
```go
type Spinner struct {
    mu      sync.RWMutex
    writer  io.Writer
    message string
    frames  []string
    done    chan bool
    active  bool
}
```

---

#### 2. Channel Panic: Sending on closed channel
**File:** `/Users/alecf/projects/aiman/internal/cli/query_executor.go`
**Lines:** 36, 46-48, 64-66, 70
**Severity:** CRITICAL
**Issue Type:** Channel Panic

**Description:**
The `executeStreamingQuery()` function can panic when sending on closed channels:

1. **Multiple sends after channel close:** If `chunkCh` is closed by the provider goroutine while the main goroutine is still processing, sending to the closed channel in line 46 will panic
2. **Incomplete channel consumption:** If an error occurs (line 64-66), the function returns without fully consuming the channels, leaving the provider goroutine running
3. **Race between channel operations:** No synchronization ensures that channel closes happen safely

**Example Problematic Code:**
```go
// Line 36 - Get channels from provider
chunkCh, errCh := provider.StreamQuery(ctx, req)

// Line 46-48 - Receive from chunk channel
case chunk, ok := <-chunkCh:
    if !ok {
        break streamLoop
    }

// Line 64-66 - Early return on error
case err := <-errCh:
    spin.Stop()
    return nil, fmt.Errorf("LLM query failed: %w", err)
    // BUG: chunkCh is still open and may still have data!
```

**Impact:**
- Runtime panic: "send on closed channel"
- Goroutine leak: Provider continues sending to abandoned channels
- Memory leak from unclosed channels

**Recommended Fix:**
Use context cancellation and ensure channels are fully drained:
```go
// Close channels on any exit path
defer func() {
    for range chunkCh {
        // Drain remaining chunks
    }
}()

select {
case chunk, ok := <-chunkCh:
    // Handle chunk
case err := <-errCh:
    spin.Stop()
    return nil, fmt.Errorf("LLM query failed: %w", err)
case <-ctx.Done():
    return nil, ctx.Err()
}
```

---

### HIGH Severity Issues

#### 3. Goroutine Leak: OpenAI StreamQuery
**File:** `/Users/alecf/projects/aiman/internal/llm/openai.go`
**Lines:** 65-122
**Severity:** HIGH
**Issue Type:** Goroutine Leak

**Description:**
The `StreamQuery()` method launches a goroutine that may not exit if the caller stops reading from the channels:

1. **No timeout:** The goroutine runs indefinitely if the caller cancels the context or stops reading
2. **Blocking sends:** Sends to `chunkCh` (lines 96-100) block indefinitely if receiver stops reading
3. **Missing context check:** The goroutine doesn't check if context is cancelled before sending

**Example Problematic Code:**
```go
// Line 88-90 - Create unbuffered channels
chunkCh := make(chan StreamChunk)
errCh := make(chan error, 1)

go func() {
    // ... (lines 92-154)
    for stream.Next() {  // LINE 88
        // ...
        chunkCh <- StreamChunk{...}  // BLOCKS if receiver stopped
    }
}()
```

**Scenario:**
1. Caller invokes `StreamQuery()`
2. Caller reads one chunk and cancels context
3. Goroutine tries to send next chunk to closed channel → deadlock

**Impact:**
- Goroutines accumulate over time
- Memory leaks from persistent goroutines
- Resource exhaustion under high query volume

**Recommended Fix:**
Add context check and buffering:
```go
// Buffer channels with context awareness
chunkCh := make(chan StreamChunk, 10)
errCh := make(chan error, 1)

go func() {
    defer close(chunkCh)
    defer close(errCh)

    for stream.Next() {
        select {
        case <-ctx.Done():
            errCh <- ctx.Err()
            return
        case chunkCh <- StreamChunk{...}:
            // sent successfully
        }
    }
}()
```

---

#### 4. Goroutine Leak: Ollama StreamQuery
**File:** `/Users/alecf/projects/aiman/internal/llm/ollama.go`
**Lines:** 88-157
**Severity:** HIGH
**Issue Type:** Goroutine Leak

**Description:**
Similar to OpenAI provider, the `StreamQuery()` method has the same goroutine leak issue:

1. **Unbuffered channels:** Created at lines 89-90
2. **Blocking sends:** Lines 131-136, 140-143 send to unbuffered channels that can block forever
3. **No context checks:** Goroutine doesn't monitor context cancellation

**Example Problematic Code:**
```go
// Line 89-90 - Unbuffered channels
chunkCh := make(chan StreamChunk)
errCh := make(chan error, 1)

go func() {
    // ...
    chunkCh <- StreamChunk{...}  // LINE 131-136, 140-143 - BLOCKING
}()

// No select with ctx.Done()
```

**Impact:**
- Same as issue #3
- Potential deadlocks under network latency

**Recommended Fix:**
Same buffering and context monitoring as OpenAI provider

---

#### 5. Channel Panic: Sending on already-closed channel
**File:** `/Users/alecf/projects/aiman/internal/llm/openai.go`
**Lines:** 104-105
**Severity:** HIGH
**Issue Type:** Channel Panic / Double Close

**Description:**
If `stream.Err()` returns an error after some chunks were sent, the error is sent to `errCh` (line 105). However, both `chunkCh` and `errCh` are deferred to close at lines 70-71. If an error occurs, this code sends to `errCh` which is then immediately closed by the defer.

More critically, if multiple errors occur or the caller stops reading, sending to the closed channel causes a panic.

**Example Problematic Code:**
```go
// Line 69-71 - Defer close (will run at function exit)
go func() {
    defer close(chunkCh)
    defer close(errCh)

    // ... streaming loop ...

    if err := stream.Err(); err != nil {
        errCh <- fmt.Errorf("stream error: %w", err)  // LINE 105
        return  // Causes deferred close to run
    }

    // LINE 113 - Another send AFTER potential error
    chunkCh <- StreamChunk{...}
}()
```

**Impact:**
- Runtime panic on second error
- Unpredictable behavior with concurrent channel operations

---

#### 6. Channel Panic: Same issue in Ollama
**File:** `/Users/alecf/projects/aiman/internal/llm/ollama.go`
**Lines:** 150-152
**Severity:** HIGH
**Issue Type:** Channel Panic / Double Close

**Description:**
Same pattern as issue #5 in the Ollama provider.

```go
// Line 93-94 - Defer close
go func() {
    defer close(chunkCh)
    defer close(errCh)

    // ... logic ...

    err := p.client.Chat(ctx, chatReq, respFunc)
    if err != nil {
        errCh <- fmt.Errorf("stream error: %w", err)  // LINE 151 - PANIC if closed
        return
    }
}()
```

**Impact:**
- Same as issue #5
- Panic when streaming fails

---

### MEDIUM Severity Issues

#### 7. Missing Synchronization: Cache concurrent access
**File:** `/Users/alecf/projects/aiman/internal/cache/cache.go`
**Lines:** 41-72
**Severity:** MEDIUM
**Issue Type:** Missing Synchronization / Race Condition

**Description:**
The `Cache` struct has no mutex to protect concurrent access to filesystem operations:

1. **Get/Set race:** If multiple goroutines call `Get()` and `Set()` simultaneously, file system operations can corrupt data
2. **TOCTOU:** Time-of-check-time-of-use race between checking file existence and reading (lines 46-52)
3. **Metadata update race:** Multiple goroutines can update the same cache entry metadata simultaneously (lines 64-66)

**Example Problematic Code:**
```go
// No synchronization mechanism
func (c *Cache) Get(command, question, model string) (*llm.QueryResponse, bool) {
    key := GenerateKey(command, question, model)
    entryPath := filepath.Join(c.cacheDir, key+".json")

    // RACE: File could be deleted between check and read
    data, err := os.ReadFile(entryPath)  // LINE 46
    // ...

    // RACE: Multiple goroutines write simultaneously
    entry.AccessedAt = time.Now()
    entry.AccessCount++
    c.saveEntry(&entry)  // LINE 66
}
```

**Scenarios:**
1. Two goroutines write to same cache file simultaneously → corrupted data
2. One goroutine deletes a file while another reads it → error
3. Concurrent metadata updates lose the last write

**Impact:**
- Cache corruption
- Lost updates to access counts
- Unpredictable cache behavior under concurrency

**Recommended Fix:**
Add sync.RWMutex to Cache struct:
```go
type Cache struct {
    mu         sync.RWMutex
    cacheDir   string
    maxAgeDays int
}

func (c *Cache) Get(...) {
    c.mu.RLock()
    defer c.mu.RUnlock()
    // ... read operations
}

func (c *Cache) Set(...) {
    c.mu.Lock()
    defer c.mu.Unlock()
    // ... write operations
}
```

---

#### 8. Missing Synchronization: Spinner done channel reuse
**File:** `/Users/alecf/projects/aiman/internal/spinner/spinner.go`
**Lines:** 74, 91
**Severity:** MEDIUM
**Issue Type:** Channel Reuse After Close

**Description:**
The `done` channel is closed multiple times if `Stop()` and `StopWithMessage()` are both called:

1. First call closes the channel (line 74 or 91)
2. Second call attempts to close already-closed channel → panic: "close of closed channel"

**Example Problematic Code:**
```go
// Line 74 - First close
func (s *Spinner) Stop() {
    if !s.active {
        return
    }
    s.active = false
    close(s.done)  // PANIC if called twice
}

// Line 91 - Second close
func (s *Spinner) StopWithMessage(message string) {
    if !s.active {
        if term.IsTerminal(int(os.Stderr.Fd())) {
            fmt.Fprintln(s.writer, message)
        }
        return
    }
    s.active = false
    close(s.done)  // PANIC: closing closed channel
}
```

**Scenario:**
```go
spin := spinner.New("Loading...")
spin.Start()
spin.Stop()        // Closes done channel
spin.Stop()        // PANIC: close of closed channel
```

**Impact:**
- Runtime panic under normal usage patterns
- Application crash

**Recommended Fix:**
Use sync.Once or check channel state:
```go
type Spinner struct {
    // ...
    stopOnce sync.Once
}

func (s *Spinner) Stop() {
    s.stopOnce.Do(func() {
        if s.active {
            s.active = false
            close(s.done)
        }
    })
}
```

---

#### 9. Potential Channel Block: Query executor receive loop
**File:** `/Users/alecf/projects/aiman/internal/cli/query_executor.go`
**Lines:** 42-68
**Severity:** MEDIUM
**Issue Type:** Channel Deadlock Potential

**Description:**
If both `chunkCh` and `errCh` close without sending final data, the select loop may hang:

1. **No timeout:** The select loop has no timeout mechanism
2. **Context not checked:** The context's Done() channel is not monitored
3. **Incomplete protocol:** No guarantee that at least one chunk or error will be sent

**Example Problematic Code:**
```go
streamLoop:
for {
    select {
    case chunk, ok := <-chunkCh:
        // LINE 46-48
    case err := <-errCh:
        // LINE 64-66
        // Missing: case <-ctx.Done()
    }
}
// Could hang indefinitely if channels close without sending
```

**Scenario:**
If the streaming provider goroutine crashes or exits without sending to either channel, the loop hangs.

**Impact:**
- Application hangs indefinitely
- User-facing timeout (query appears stuck)
- Resource exhaustion from hanging processes

**Recommended Fix:**
Monitor context:
```go
select {
case chunk, ok := <-chunkCh:
    // ...
case err := <-errCh:
    // ...
case <-ctx.Done():
    return nil, ctx.Err()
}
```

---

### LOW Severity Issues

#### 10. Unprotected global state: Viper configuration
**File:** `/Users/alecf/projects/aiman/internal/cli/root.go`
**Lines:** 68-75
**Severity:** LOW
**Issue Type:** Global State Access

**Description:**
Viper is used for global configuration access without synchronization. While Viper has internal locks, it's still global state that could be problematic in concurrent scenarios.

**Example:**
```go
viper.BindPFlag("profile", rootCmd.PersistentFlags().Lookup("profile"))
viper.AutomaticEnv()
```

**Impact:**
- Low risk in current single-threaded CLI usage
- Could cause issues if CLI is made concurrent in future
- Good practice to avoid global state

---

## Summary Table

| # | File | Line(s) | Issue | Severity | Type |
|---|------|---------|-------|----------|------|
| 1 | spinner.go | 18,38,57-58,70,73 | Race condition: active & message | CRITICAL | Race Condition |
| 2 | query_executor.go | 36,46-48,64-66,70 | Channel panic on close | CRITICAL | Channel Panic |
| 3 | openai.go | 65-122 | Goroutine leak in StreamQuery | HIGH | Goroutine Leak |
| 4 | ollama.go | 88-157 | Goroutine leak in StreamQuery | HIGH | Goroutine Leak |
| 5 | openai.go | 104-105 | Channel double close | HIGH | Channel Panic |
| 6 | ollama.go | 150-152 | Channel double close | HIGH | Channel Panic |
| 7 | cache.go | 41-72 | Race: concurrent cache access | MEDIUM | Missing Sync |
| 8 | spinner.go | 74,91 | Double close of done channel | MEDIUM | Channel Panic |
| 9 | query_executor.go | 42-68 | No context monitoring in select | MEDIUM | Deadlock Potential |
| 10 | root.go | 68-75 | Global state (Viper) | LOW | Global State |

---

## Remediation Priority

### Immediate Action Required (CRITICAL)
1. Fix spinner race conditions (Issue #1)
2. Fix query executor channel panics (Issue #2)

### High Priority (within 1 week)
3. Fix OpenAI and Ollama goroutine leaks (Issues #3, #4)
4. Fix channel double-close panics (Issues #5, #6)
5. Fix spinner double-close panic (Issue #8)

### Medium Priority (within 1 month)
6. Fix cache synchronization (Issue #7)
7. Add context monitoring to query executor (Issue #9)

### Low Priority (refactoring)
8. Reduce reliance on global state (Issue #10)

---

## Testing Recommendations

1. **Race detector:** Run with `-race` flag during testing:
   ```bash
   go test -race ./...
   ```

2. **Concurrency tests:** Create stress tests that:
   - Start multiple spinners concurrently
   - Query multiple LLM providers simultaneously
   - Rapidly start/stop spinners
   - Cancel operations mid-stream

3. **Channel tests:** Verify:
   - Channels are properly closed in all paths
   - No panics on early returns
   - No goroutine leaks under cancellation

---

## References

- Go Concurrency Patterns: https://go.dev/blog/pipelines
- Go Memory Model: https://go.dev/ref/mem
- Data Race Detector: https://go.dev/doc/articles/race_detector
