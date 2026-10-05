package spinner

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestNotTTYWritesNothing(t *testing.T) {
	var buf syncBuffer
	s := newSpinner(&buf, false, 0, "hi")
	s.Start()
	s.Update("x")
	time.Sleep(100 * time.Millisecond)
	s.Stop()
	s.StopWithMessage("done")
	if buf.String() != "" {
		t.Errorf("wrote %q to a non-terminal", buf.String())
	}
}

func TestStopWithoutStartAndDoubleStop(t *testing.T) {
	var buf syncBuffer
	s := newSpinner(&buf, true, 0, "hi")
	s.Stop() // never started: no-op
	if buf.String() != "" {
		t.Errorf("Stop without Start wrote %q", buf.String())
	}
	s.Start()
	s.Stop()
	s.Stop()
	if !strings.HasSuffix(buf.String(), clearLine) {
		t.Errorf("output doesn't end with a line clear: %q", buf.String())
	}
}

func TestNothingWrittenAfterStop(t *testing.T) {
	for i := 0; i < 20; i++ {
		var buf syncBuffer
		s := newSpinner(&buf, true, 0, "working")
		s.Start()
		time.Sleep(time.Duration(i%5) * 30 * time.Millisecond)
		s.Stop()
		after := buf.String()
		time.Sleep(120 * time.Millisecond)
		if buf.String() != after {
			t.Fatal("spinner wrote after Stop returned")
		}
		if !strings.HasSuffix(after, clearLine) {
			t.Fatalf("last write isn't a line clear: %q", after[max(0, len(after)-20):])
		}
	}
}

func TestConcurrentUpdateStop(t *testing.T) {
	var buf syncBuffer
	s := newSpinner(&buf, true, 0, "x")
	s.Start()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Update("msg")
			}
			s.Stop()
		}()
	}
	wg.Wait()
}

func TestRestart(t *testing.T) {
	var buf syncBuffer
	s := newSpinner(&buf, true, 0, "x")
	s.Start()
	s.Start() // second Start is a no-op
	s.Stop()
	s.Start()
	s.Update("again")
	s.Stop()
	if !strings.Contains(buf.String(), "again") {
		t.Error("restarted spinner didn't draw")
	}
}

func TestTruncatesToWidth(t *testing.T) {
	var buf syncBuffer
	s := newSpinner(&buf, true, 20, strings.Repeat("long message ", 10))
	s.Start()
	s.Stop()
	for _, line := range strings.Split(buf.String(), clearLine) {
		if n := len([]rune(line)); n > 19 {
			t.Errorf("line of %d runes exceeds width: %q", n, line)
		}
	}
}

func TestStopWithMessage(t *testing.T) {
	var buf syncBuffer
	s := newSpinner(&buf, true, 0, "x")
	s.Start()
	s.StopWithMessage("done")
	if !strings.HasSuffix(buf.String(), clearLine+"done\n") {
		t.Errorf("got %q", buf.String())
	}
}
