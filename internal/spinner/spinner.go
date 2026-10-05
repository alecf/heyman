// Package spinner draws a progress indicator on stderr while heyman waits for
// a model. It only draws when stderr is a terminal, so it never pollutes
// redirected output.
package spinner

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

const clearLine = "\r\033[K"

// Spinner displays an animated progress indicator. It is safe to call
// Update and Stop from multiple goroutines, to Stop more than once, and to
// Stop a spinner that was never started.
type Spinner struct {
	writer io.Writer
	tty    bool
	width  int // terminal width; messages are truncated to fit
	frames []string

	mu      sync.Mutex // guards message, active, frame and all writes
	message string
	active  bool
	frame   int
	done    chan struct{}
	stopped chan struct{}
}

// New creates a spinner that writes to stderr.
func New(message string) *Spinner {
	fd := int(os.Stderr.Fd())
	tty := term.IsTerminal(fd)
	width := 0
	if tty {
		if w, _, err := term.GetSize(fd); err == nil {
			width = w
		}
	}
	return newSpinner(os.Stderr, tty, width, message)
}

func newSpinner(w io.Writer, tty bool, width int, message string) *Spinner {
	return &Spinner{
		writer:  w,
		tty:     tty,
		width:   width,
		message: message,
		frames:  []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	}
}

// Start begins the animation. It does nothing if the output isn't a
// terminal or the spinner is already running.
func (s *Spinner) Start() {
	if !s.tty {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return
	}
	s.active = true
	s.done = make(chan struct{})
	s.stopped = make(chan struct{})
	s.drawLocked()

	go func(done, stopped chan struct{}) {
		defer close(stopped)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s.mu.Lock()
				if s.active {
					s.frame = (s.frame + 1) % len(s.frames)
					s.drawLocked()
				}
				s.mu.Unlock()
			}
		}
	}(s.done, s.stopped)
}

// drawLocked redraws the current frame. s.mu must be held.
func (s *Spinner) drawLocked() {
	line := s.frames[s.frame] + " " + s.message
	if s.width > 1 {
		// A line that wraps can't be cleared with \r, so keep it on one row.
		r := []rune(line)
		if len(r) > s.width-1 {
			line = string(r[:s.width-2]) + "…"
		}
	}
	fmt.Fprint(s.writer, clearLine+line)
}

// Update changes the spinner message.
func (s *Spinner) Update(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message = message
	if s.active {
		s.drawLocked()
	}
}

// Stop halts the spinner and clears its line. After Stop returns nothing
// more is written.
func (s *Spinner) Stop() {
	s.stop("")
}

// StopWithMessage stops the spinner and prints message on its own line (on
// a terminal only).
func (s *Spinner) StopWithMessage(message string) {
	s.stop(message)
}

func (s *Spinner) stop(message string) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		if s.tty && message != "" {
			fmt.Fprintln(s.writer, message)
		}
		return
	}
	s.active = false
	done, stopped := s.done, s.stopped
	s.mu.Unlock()

	close(done)
	<-stopped // the animation goroutine has exited; no frame can follow

	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(s.writer, clearLine)
	if message != "" {
		fmt.Fprintln(s.writer, message)
	}
}
