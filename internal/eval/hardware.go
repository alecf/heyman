package eval

import (
	"bufio"
	"context"
	"os"
	"os/exec"
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
	MemoryGB float64 `json:"memory_gb"`
	Context  int     `json:"context,omitempty"`
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
				s.mu.Lock()
				for name, m := range ParseOllamaPS(string(out)) {
					if m.MemoryGB > s.peak[name].MemoryGB {
						s.peak[name] = m
					}
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
