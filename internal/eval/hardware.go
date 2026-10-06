package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hardware describes the machine a run happened on.
type Hardware struct {
	CPU      string  `json:"cpu,omitempty"`
	MemoryGB float64 `json:"memory_gb,omitempty"`
	OS       string  `json:"os,omitempty"`
}

// DetectHardware returns the CPU model, installed RAM and OS.
func DetectHardware() Hardware {
	h := Hardware{OS: OSDescription()}
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			h.CPU = strings.TrimSpace(string(out))
		}
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if b, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil {
				h.MemoryGB = b / (1 << 30)
			}
		}
	case "linux":
		if f, err := os.Open("/proc/cpuinfo"); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
					h.CPU = strings.TrimSpace(v)
					break
				}
			}
			f.Close()
		}
		if f, err := os.Open("/proc/meminfo"); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if fields := strings.Fields(sc.Text()); len(fields) >= 2 && fields[0] == "MemTotal:" {
					if kb, err := strconv.ParseFloat(fields[1], 64); err == nil {
						h.MemoryGB = kb / (1 << 20)
					}
					break
				}
			}
			f.Close()
		}
	}
	return h
}

// OllamaVersion returns the local Ollama version, or "".
func OllamaVersion() string {
	out, err := exec.Command("ollama", "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	// "ollama version is 0.33.1" (possibly preceded by a client warning line).
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ollama version is "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// LoadedModel is one row of `ollama ps`.
type LoadedModel struct {
	// MemoryGB is the size Ollama reports. It can badly undercount: for
	// Gemma 4 it omits the memory-mapped weights.
	MemoryGB float64 `json:"memory_gb"`
	Context  int     `json:"context,omitempty"`
	// ResidentGB is the peak resident memory of Ollama's model runner
	// process(es) while this was the only model loaded. Prefer it.
	ResidentGB float64 `json:"resident_gb,omitempty"`
}

// ParseOllamaPS parses `ollama ps` output into model name → size/context.
// Columns: NAME ID SIZE(e.g. "6.7 GB") PROCESSOR(e.g. "100% GPU") CONTEXT UNTIL…
func ParseOllamaPS(out string) map[string]LoadedModel {
	res := map[string]LoadedModel{}
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 4 {
			continue
		}
		size, err := strconv.ParseFloat(f[2], 64)
		if err != nil {
			continue
		}
		switch strings.ToUpper(f[3]) {
		case "MB":
			size /= 1024
		case "GB":
		default:
			continue
		}
		m := LoadedModel{MemoryGB: size}
		// PROCESSOR is two fields ("100% GPU") or four ("48%/52% CPU/GPU");
		// CONTEXT is the first plain integer after it.
		for _, v := range f[4:] {
			if n, err := strconv.Atoi(v); err == nil {
				m.Context = n
				break
			}
		}
		res[f[0]] = m
	}
	return res
}

// OllamaSampler polls `ollama ps` and keeps each model's peak memory.
type OllamaSampler struct {
	mu   sync.Mutex
	peak map[string]LoadedModel
	stop context.CancelFunc
	done chan struct{}
}

// StartOllamaSampler samples every interval until Stop is called.
func StartOllamaSampler(ctx context.Context, interval time.Duration) *OllamaSampler {
	ctx, cancel := context.WithCancel(ctx)
	s := &OllamaSampler{peak: map[string]LoadedModel{}, stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if out, err := exec.CommandContext(ctx, "ollama", "ps").Output(); err == nil {
				loaded := ParseOllamaPS(string(out))
				// Runner RSS can only be attributed when one model is loaded.
				rss := 0.0
				if len(loaded) == 1 {
					rss = runnerResidentGB(ctx)
				}
				s.mu.Lock()
				for name, m := range loaded {
					p := s.peak[name]
					if m.MemoryGB > p.MemoryGB {
						p.MemoryGB, p.Context = m.MemoryGB, m.Context
					}
					if rss > p.ResidentGB {
						p.ResidentGB = rss
					}
					s.peak[name] = p
				}
				s.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return s
}

// Stop ends sampling and returns peak usage keyed by "ollama/<name>".
func (s *OllamaSampler) Stop() map[string]LoadedModel {
	s.stop()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]LoadedModel, len(s.peak))
	for name, m := range s.peak {
		out["ollama/"+name] = m
	}
	return out
}

// Warmup records how a local model was prepared before its first attempt.
type Warmup struct {
	LoadSeconds float64  `json:"load_seconds"`
	Unloaded    []string `json:"unloaded,omitempty"`
}

// PrepareOllama unloads every other loaded Ollama model, then loads model
// (a "ollama/<name>" spec) and keeps it resident, so the first timed attempt
// doesn't pay the load and nothing else competes for memory. baseURL is the
// server root, e.g. "http://localhost:11434".
func PrepareOllama(ctx context.Context, baseURL, model string) (Warmup, error) {
	var w Warmup
	name := strings.TrimPrefix(model, "ollama/")
	if out, err := exec.CommandContext(ctx, "ollama", "ps").Output(); err == nil {
		for other := range ParseOllamaPS(string(out)) {
			if other == name {
				continue
			}
			if err := exec.CommandContext(ctx, "ollama", "stop", other).Run(); err == nil {
				w.Unloaded = append(w.Unloaded, other)
			}
		}
	}
	// An empty prompt loads the model without generating. keep_alive covers
	// gaps between attempts (slow cases, the judge, exec checks).
	body, _ := json.Marshal(map[string]any{"model": name, "keep_alive": "30m"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return w, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return w, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	w.LoadSeconds = time.Since(start).Seconds()
	if resp.StatusCode != http.StatusOK {
		return w, fmt.Errorf("loading %s: %s", name, resp.Status)
	}
	return w, nil
}

// runnerResidentGB sums the resident memory of Ollama's model runner
// processes (llama-server, or "ollama runner" in older versions).
func runnerResidentGB(ctx context.Context) float64 {
	out, err := exec.CommandContext(ctx, "ps", "-Ao", "rss=,command=").Output()
	if err != nil {
		return 0
	}
	return sumRunnerRSS(string(out))
}

func sumRunnerRSS(psOut string) float64 {
	var kb float64
	for _, line := range strings.Split(psOut, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		prog := filepath.Base(f[1])
		isRunner := prog == "llama-server" || (prog == "ollama" && len(f) > 2 && f[2] == "runner")
		if !isRunner {
			continue
		}
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			kb += v
		}
	}
	return kb / (1 << 20)
}
