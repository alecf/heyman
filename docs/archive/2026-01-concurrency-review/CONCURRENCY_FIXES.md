# Concurrency Issues - Code Fixes

This document provides detailed code fixes for all identified concurrency issues in the heyman project.

---

## Issue #1: Spinner Race Condition (CRITICAL)

### Problem
The `Spinner` struct has race conditions on the `active` and `message` fields.

### Current Code
```go
type Spinner struct {
    writer  io.Writer
    message string      // Race: written from Update(), read from goroutine
    frames  []string
    done    chan bool
    active  bool         // Race: written from Start/Stop, checked everywhere
}

func (s *Spinner) Start() {
    s.active = true      // WRITE - not synchronized
    go func() {
        // ...
        fmt.Fprintf(s.writer, "\r%s %s", s.frames[frame], s.message) // READ - not synchronized
    }()
}

func (s *Spinner) Update(message string) {
    s.message = message  // WRITE - not synchronized
}

func (s *Spinner) Stop() {
    if !s.active {       // READ - not synchronized
        return
    }
    s.active = false     // WRITE - not synchronized
    close(s.done)
}
```

### Fixed Code
```go
import "sync"

type Spinner struct {
    mu      sync.RWMutex // Add mutex
    writer  io.Writer
    message string
    frames  []string
    done    chan bool
    active  bool
}

func (s *Spinner) Start() {
    s.mu.Lock()
    s.active = true
    s.mu.Unlock()

    go func() {
        ticker := time.NewTicker(80 * time.Millisecond)
        defer ticker.Stop()

        frame := 0
        for {
            select {
            case <-s.done:
                return
            case <-ticker.C:
                s.mu.RLock()
                msg := s.message
                s.mu.RUnlock()
                fmt.Fprintf(s.writer, "\r%s %s", s.frames[frame], msg)
                frame = (frame + 1) % len(s.frames)
            }
        }
    }()
}

func (s *Spinner) Update(message string) {
    s.mu.Lock()
    s.message = message
    isActive := s.active
    s.mu.Unlock()

    if !isActive {
        return
    }

    if term.IsTerminal(int(os.Stderr.Fd())) {
        fmt.Fprintf(s.writer, "\r\033[K%s %s", s.frames[0], message)
    }
}

func (s *Spinner) Stop() {
    s.mu.Lock()
    if !s.active {
        s.mu.Unlock()
        return
    }
    s.active = false
    s.mu.Unlock()

    close(s.done)

    if term.IsTerminal(int(os.Stderr.Fd())) {
        fmt.Fprintf(s.writer, "\r\033[K")
    }
}

func (s *Spinner) StopWithMessage(message string) {
    s.mu.Lock()
    if !s.active {
        s.mu.Unlock()
        if term.IsTerminal(int(os.Stderr.Fd())) {
            fmt.Fprintln(s.writer, message)
        }
        return
    }
    s.active = false
    s.mu.Unlock()

    close(s.done)

    if term.IsTerminal(int(os.Stderr.Fd())) {
        fmt.Fprintf(s.writer, "\r\033[K%s\n", message)
    }
}
```

---

## Issue #2: Query Executor Channel Panic (CRITICAL)

### Problem
Channels can be closed while the consumer is still trying to receive, causing panics.

### Current Code
```go
func executeStreamingQuery(ctx context.Context, provider llm.Provider, req llm.QueryRequest, opts QueryOptions) (*llm.QueryResponse, error) {
    spin := spinner.New(fmt.Sprintf("Sending query to %s...", opts.Profile.Model))
    spin.Start()

    chunkCh, errCh := provider.StreamQuery(ctx, req)

    var content strings.Builder
    var tokenInput, tokenOutput int
    firstChunk := true

    // Problem: No context monitoring, no guarantee channels will send
streamLoop:
    for {
        select {
        case chunk, ok := <-chunkCh:
            if !ok {
                break streamLoop
            }

            if firstChunk && !chunk.IsComplete {
                firstChunk = false
                spin.Update(fmt.Sprintf("Getting command from %s...", opts.Profile.Model))
            }

            if chunk.IsComplete {
                tokenInput = chunk.TokensInput
                tokenOutput = chunk.TokensOutput
                break streamLoop
            }

            content.WriteString(chunk.Content)

        case err := <-errCh:
            spin.Stop()
            return nil, fmt.Errorf("LLM query failed: %w", err)
            // BUG: Channels still open, goroutine still running
        }
    }

    spin.Stop()
    // ...
}
```

### Fixed Code
```go
func executeStreamingQuery(ctx context.Context, provider llm.Provider, req llm.QueryRequest, opts QueryOptions) (*llm.QueryResponse, error) {
    spin := spinner.New(fmt.Sprintf("Sending query to %s...", opts.Profile.Model))
    spin.Start()
    defer spin.Stop()

    chunkCh, errCh := provider.StreamQuery(ctx, req)

    var content strings.Builder
    var tokenInput, tokenOutput int
    firstChunk := true

    // Drain remaining channels on exit
    defer func() {
        for range chunkCh {
            // Drain any remaining chunks
        }
        for range errCh {
            // Drain any remaining errors
        }
    }()

streamLoop:
    for {
        select {
        case chunk, ok := <-chunkCh:
            if !ok {
                break streamLoop
            }

            if firstChunk && !chunk.IsComplete {
                firstChunk = false
                spin.Update(fmt.Sprintf("Getting command from %s...", opts.Profile.Model))
            }

            if chunk.IsComplete {
                tokenInput = chunk.TokensInput
                tokenOutput = chunk.TokensOutput
                break streamLoop
            }

            content.WriteString(chunk.Content)

        case err := <-errCh:
            return nil, fmt.Errorf("LLM query failed: %w", err)

        case <-ctx.Done():
            return nil, ctx.Err()
        }
    }

    return &llm.QueryResponse{
        Content:      content.String(),
        TokensInput:  tokenInput,
        TokensOutput: tokenOutput,
        Model:        req.Model,
        Provider:     provider.Name(),
        Cached:       false,
    }, nil
}
```

---

## Issue #3 & #4: Goroutine Leaks in StreamQuery (HIGH)

### Problem
Unbuffered channels block indefinitely if the caller stops reading.

### Current Code (OpenAI - applies to Ollama too)
```go
func (p *OpenAIProvider) StreamQuery(ctx context.Context, req QueryRequest) (<-chan StreamChunk, <-chan error) {
    chunkCh := make(chan StreamChunk)    // Unbuffered - can block
    errCh := make(chan error, 1)

    go func() {
        defer close(chunkCh)
        defer close(errCh)

        // ... setup ...

        for stream.Next() {
            chunk := stream.Current()
            acc.AddChunk(chunk)

            if len(chunk.Choices) > 0 {
                delta := chunk.Choices[0].Delta
                if delta.Content != "" {
                    chunkCh <- StreamChunk{  // BLOCKS if receiver stopped
                        Content:    delta.Content,
                        IsComplete: false,
                    }
                }
            }
        }

        if err := stream.Err(); err != nil {
            errCh <- fmt.Errorf("stream error: %w", err)  // Can block
            return
        }

        chunkCh <- StreamChunk{
            Content:      "",
            IsComplete:   true,
            TokensInput:  inputTokens,
            TokensOutput: outputTokens,
        }  // Can block
    }()

    return chunkCh, errCh
}
```

### Fixed Code
```go
func (p *OpenAIProvider) StreamQuery(ctx context.Context, req QueryRequest) (<-chan StreamChunk, <-chan error) {
    chunkCh := make(chan StreamChunk, 10)  // Buffer to prevent blocking
    errCh := make(chan error, 1)

    go func() {
        defer close(chunkCh)
        defer close(errCh)

        // ... setup ...

        for stream.Next() {
            // Check if context is cancelled before processing
            select {
            case <-ctx.Done():
                errCh <- ctx.Err()
                return
            default:
            }

            chunk := stream.Current()
            acc.AddChunk(chunk)

            if len(chunk.Choices) > 0 {
                delta := chunk.Choices[0].Delta
                if delta.Content != "" {
                    // Use select to allow context cancellation
                    select {
                    case chunkCh <- StreamChunk{
                        Content:    delta.Content,
                        IsComplete: false,
                    }:
                        // Sent successfully
                    case <-ctx.Done():
                        errCh <- ctx.Err()
                        return
                    }
                }
            }
        }

        if err := stream.Err(); err != nil {
            select {
            case errCh <- fmt.Errorf("stream error: %w", err):
            case <-ctx.Done():
                errCh <- ctx.Err()
            }
            return
        }

        inputTokens := int(acc.Usage.PromptTokens)
        outputTokens := int(acc.Usage.CompletionTokens)

        select {
        case chunkCh <- StreamChunk{
            Content:      "",
            IsComplete:   true,
            TokensInput:  inputTokens,
            TokensOutput: outputTokens,
        }:
            // Sent successfully
        case <-ctx.Done():
            errCh <- ctx.Err()
        }
    }()

    return chunkCh, errCh
}
```

### Apply same fix to Ollama provider
```go
func (p *OllamaProvider) StreamQuery(ctx context.Context, req QueryRequest) (<-chan StreamChunk, <-chan error) {
    chunkCh := make(chan StreamChunk, 10)  // Buffer
    errCh := make(chan error, 1)

    go func() {
        defer close(chunkCh)
        defer close(errCh)

        // ... setup ...

        respFunc := func(resp api.ChatResponse) error {
            // Check context inside callback
            select {
            case <-ctx.Done():
                return ctx.Err()
            default:
            }

            if resp.Done {
                promptTokens = resp.PromptEvalCount
                completionTokens = resp.EvalCount

                select {
                case chunkCh <- StreamChunk{
                    Content:      "",
                    IsComplete:   true,
                    TokensInput:  promptTokens,
                    TokensOutput: completionTokens,
                }:
                case <-ctx.Done():
                    return ctx.Err()
                }
            } else {
                if resp.Message.Content != "" {
                    select {
                    case chunkCh <- StreamChunk{
                        Content:    resp.Message.Content,
                        IsComplete: false,
                    }:
                    case <-ctx.Done():
                        return ctx.Err()
                    }
                }
            }
            return nil
        }

        err := p.client.Chat(ctx, chatReq, respFunc)
        if err != nil {
            select {
            case errCh <- fmt.Errorf("stream error: %w", err):
            case <-ctx.Done():
                errCh <- ctx.Err()
            }
            return
        }
    }()

    return chunkCh, errCh
}
```

---

## Issue #8: Spinner Double Close (MEDIUM)

### Problem
Both `Stop()` and `StopWithMessage()` close the same channel.

### Current Code
```go
func (s *Spinner) Stop() {
    if !s.active {
        return
    }
    s.active = false
    close(s.done)  // CLOSE #1
}

func (s *Spinner) StopWithMessage(message string) {
    if !s.active {
        if term.IsTerminal(int(os.Stderr.Fd())) {
            fmt.Fprintln(s.writer, message)
        }
        return
    }
    s.active = false
    close(s.done)  // CLOSE #2 - PANIC if called after Stop()
}
```

### Fixed Code
```go
import "sync"

type Spinner struct {
    writer    io.Writer
    message   string
    frames    []string
    done      chan bool
    active    bool
    stopOnce  sync.Once  // Add once
}

func (s *Spinner) Stop() {
    if !s.active {
        return
    }
    s.active = false

    s.stopOnce.Do(func() {
        close(s.done)
    })

    if term.IsTerminal(int(os.Stderr.Fd())) {
        fmt.Fprintf(s.writer, "\r\033[K")
    }
}

func (s *Spinner) StopWithMessage(message string) {
    if !s.active {
        if term.IsTerminal(int(os.Stderr.Fd())) {
            fmt.Fprintln(s.writer, message)
        }
        return
    }
    s.active = false

    s.stopOnce.Do(func() {
        close(s.done)
    })

    if term.IsTerminal(int(os.Stderr.Fd())) {
        fmt.Fprintf(s.writer, "\r\033[K%s\n", message)
    }
}
```

---

## Issue #7: Cache Race Condition (MEDIUM)

### Problem
Multiple goroutines can read/write cache files simultaneously.

### Current Code
```go
type Cache struct {
    cacheDir   string
    maxAgeDays int
}

func (c *Cache) Get(command, question, model string) (*llm.QueryResponse, bool) {
    key := GenerateKey(command, question, model)
    entryPath := filepath.Join(c.cacheDir, key+".json")

    // RACE: Multiple reads/writes to same file
    data, err := os.ReadFile(entryPath)
    if err != nil {
        return nil, false
    }

    var entry Entry
    if err := json.Unmarshal(data, &entry); err != nil {
        return nil, false
    }

    if c.isExpired(entry.CreatedAt) {
        os.Remove(entryPath)  // RACE: File operations
        return nil, false
    }

    entry.AccessedAt = time.Now()
    entry.AccessCount++
    c.saveEntry(&entry)  // RACE: Write without synchronization

    response := entry.Response
    response.Cached = true

    return response, true
}

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

    return c.saveEntry(entry)  // RACE: Concurrent writes
}
```

### Fixed Code
```go
import "sync"

type Cache struct {
    mu         sync.RWMutex  // Add mutex
    cacheDir   string
    maxAgeDays int
}

func (c *Cache) Get(command, question, model string) (*llm.QueryResponse, bool) {
    c.mu.RLock()
    defer c.mu.RUnlock()

    key := GenerateKey(command, question, model)
    entryPath := filepath.Join(c.cacheDir, key+".json")

    data, err := os.ReadFile(entryPath)
    if err != nil {
        return nil, false
    }

    var entry Entry
    if err := json.Unmarshal(data, &entry); err != nil {
        return nil, false
    }

    if c.isExpired(entry.CreatedAt) {
        os.Remove(entryPath)
        return nil, false
    }

    // Need to upgrade to write lock for metadata update
    c.mu.RUnlock()
    c.mu.Lock()

    entry.AccessedAt = time.Now()
    entry.AccessCount++
    err = c.saveEntryLocked(&entry)  // Locked version

    c.mu.Unlock()
    c.mu.RLock()  // Restore read lock for read defer

    if err != nil {
        return nil, false
    }

    response := entry.Response
    response.Cached = true

    return response, true
}

func (c *Cache) Set(command, question, model string, response *llm.QueryResponse) error {
    c.mu.Lock()
    defer c.mu.Unlock()

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

    return c.saveEntryLocked(entry)
}

// saveEntryLocked assumes lock is already held
func (c *Cache) saveEntryLocked(entry *Entry) error {
    if err := os.MkdirAll(c.cacheDir, 0755); err != nil {
        return fmt.Errorf("failed to create cache directory: %w", err)
    }

    entryPath := filepath.Join(c.cacheDir, entry.Key+".json")
    data, err := json.MarshalIndent(entry, "", "  ")
    if err != nil {
        return fmt.Errorf("failed to marshal cache entry: %w", err)
    }

    if err := os.WriteFile(entryPath, data, 0644); err != nil {
        return fmt.Errorf("failed to write cache entry: %w", err)
    }

    return nil
}

// Keep saveEntry for public calls
func (c *Cache) saveEntry(entry *Entry) error {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.saveEntryLocked(entry)
}
```

---

## Issue #9: Add Context Monitoring (MEDIUM)

### Already Fixed in Issue #2
See the fixed query_executor.go code above - it includes:
```go
case <-ctx.Done():
    return nil, ctx.Err()
```

---

## Testing Recommendations

### Test for Issue #1 (Spinner Race)
```go
func TestSpinnerConcurrentAccess(t *testing.T) {
    spin := New("test")
    spin.Start()

    done := make(chan struct{})

    // Multiple goroutines updating simultaneously
    for i := 0; i < 10; i++ {
        go func(id int) {
            for j := 0; j < 100; j++ {
                spin.Update(fmt.Sprintf("msg %d-%d", id, j))
            }
            done <- struct{}{}
        }(i)
    }

    for i := 0; i < 10; i++ {
        <-done
    }

    spin.Stop()
}
```

### Test for Issue #2 (Channel Panic)
```go
func TestQueryExecutorEarlyCancel(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())

    // Cancel immediately
    cancel()

    // Should not panic
    _, err := executeStreamingQuery(ctx, provider, req, opts)
    if err != context.Canceled {
        t.Errorf("expected context.Canceled, got %v", err)
    }
}
```

### Test for Issue #7 (Cache Race)
```go
func TestCacheConcurrentAccess(t *testing.T) {
    c := New(30)

    done := make(chan struct{})

    for i := 0; i < 10; i++ {
        go func(id int) {
            for j := 0; j < 100; j++ {
                cmd := fmt.Sprintf("cmd%d", id)
                q := fmt.Sprintf("q%d", j)
                resp := &QueryResponse{Content: fmt.Sprintf("resp%d-%d", id, j)}
                c.Set(cmd, q, "model", resp)
                c.Get(cmd, q, "model")
            }
            done <- struct{}{}
        }(i)
    }

    for i := 0; i < 10; i++ {
        <-done
    }
}
```

Run with race detector:
```bash
go test -race ./...
```

---

## Deployment Checklist

- [ ] Apply fix for Issue #1 (Spinner mutex)
- [ ] Apply fix for Issue #2 (Query executor context)
- [ ] Apply fix for Issue #3 (OpenAI buffering)
- [ ] Apply fix for Issue #4 (Ollama buffering)
- [ ] Apply fix for Issue #8 (Spinner sync.Once)
- [ ] Apply fix for Issue #7 (Cache mutex)
- [ ] Add concurrency tests
- [ ] Run `go test -race ./...` and verify no races
- [ ] Stress test with high concurrency
- [ ] Load test with multiple concurrent queries
