package spinner

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"
)

// Spinner displays animated progress indicators
type Spinner struct {
	writer  io.Writer
	message string
	frames  []string
	done    chan bool
	active  bool
}

// New creates a new spinner
func New(message string) *Spinner {
	return &Spinner{
		writer:  os.Stderr,
		message: message,
		frames:  []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		done:    make(chan bool),
	}
}

// Start begins the spinner animation
func (s *Spinner) Start() {
	// Only show spinner if output is to a terminal
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}

	s.active = true
	go func() {
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()

		frame := 0
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				fmt.Fprintf(s.writer, "\r%s %s", s.frames[frame], s.message)
				frame = (frame + 1) % len(s.frames)
			}
		}
	}()
}

// Update changes the spinner message
func (s *Spinner) Update(message string) {
	s.message = message
	if !s.active {
		return
	}
	// Clear and redraw immediately
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintf(s.writer, "\r\033[K%s %s", s.frames[0], s.message)
	}
}

// Stop halts the spinner and clears the line
func (s *Spinner) Stop() {
	if !s.active {
		return
	}
	s.active = false
	close(s.done)

	// Clear the line
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintf(s.writer, "\r\033[K")
	}
}

// StopWithMessage stops the spinner and displays a final message
func (s *Spinner) StopWithMessage(message string) {
	if !s.active {
		if term.IsTerminal(int(os.Stderr.Fd())) {
			fmt.Fprintln(s.writer, message)
		}
		return
	}
	s.active = false
	close(s.done)

	// Clear the line and print final message
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintf(s.writer, "\r\033[K%s\n", message)
	}
}
