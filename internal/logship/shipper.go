package logship

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/liftbay/runner/internal/api"
)

// The API accepts up to 1000 lines of up to 16 KiB per request.
const (
	maxLineLen  = 16_000
	apiMaxBatch = 1000
)

type Sink interface {
	SendLogs(ctx context.Context, lines []api.LogLine) (cancelRequested bool, err error)
	// Heartbeat tells the API the runner is alive during quiet steps, and learns about cancel requests.
	Heartbeat(ctx context.Context) (cancelRequested bool, err error)
}

// Shipper buffers lines and sends them in order from a single goroutine. A failed batch stays at
// the front of the buffer and is retried, so lines are never reordered or dropped while running.
type Shipper struct {
	sink     Sink
	redactor *Redactor
	echo     io.Writer
	// OnCancel runs when the API reports that a cancel was requested.
	OnCancel func()

	FlushEvery time.Duration
	MaxBatch   int
	Backoff    time.Duration
	// HeartbeatEvery is the longest the API goes without hearing from the runner. The API ends
	// builds whose runner stopped reporting, so quiet steps (a long compile) must still check in.
	HeartbeatEvery time.Duration

	mu      sync.Mutex
	buf     []api.LogLine
	kick    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	started bool
	lastErr error
	// Last successful contact with the API. Only the shipper goroutine touches it.
	lastContact time.Time
}

func New(sink Sink, r *Redactor, echo io.Writer) *Shipper {
	return &Shipper{sink: sink, redactor: r, echo: echo, FlushEvery: time.Second, MaxBatch: 500, Backoff: 500 * time.Millisecond, HeartbeatEvery: time.Minute,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

func (s *Shipper) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.loop()
}

// Log redacts and queues one line. It never blocks on the network.
func (s *Shipper) Log(phase, stream, text string) {
	text = s.redactor.Redact(text)
	if len(text) > maxLineLen {
		text = text[:maxLineLen] + " …(truncated)"
	}
	if s.echo != nil {
		prefix := ""
		if stream == api.Stderr {
			prefix = "! "
		}
		fmt.Fprintf(s.echo, "%s%s\n", prefix, text)
	}
	s.mu.Lock()
	s.buf = append(s.buf, api.LogLine{Phase: phase, Stream: stream, Text: text})
	full := len(s.buf) >= s.batchSize()
	s.mu.Unlock()
	if full {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

func (s *Shipper) Logf(phase, stream, format string, args ...any) {
	s.Log(phase, stream, fmt.Sprintf(format, args...))
}

func (s *Shipper) batchSize() int {
	return min(max(s.MaxBatch, 1), apiMaxBatch)
}

func (s *Shipper) loop() {
	defer close(s.done)
	t := time.NewTicker(s.FlushEvery)
	defer t.Stop()
	failures := 0
	s.lastContact = time.Now()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		case <-s.kick:
		}
		s.heartbeatIfQuiet()
		for {
			sent, err := s.flushOnce(context.Background())
			if err != nil {
				failures++
				// Back off without losing the batch; the next tick or kick retries it.
				select {
				case <-s.stop:
					return
				case <-time.After(min(s.Backoff*time.Duration(1<<min(failures, 6)), 30*time.Second)):
				}
				break
			}
			failures = 0
			if sent < s.batchSize() {
				break
			}
		}
	}
}

// heartbeatIfQuiet checks in when nothing has been sent for HeartbeatEvery. Failures are retried on a later tick.
func (s *Shipper) heartbeatIfQuiet() {
	s.mu.Lock()
	pending := len(s.buf)
	s.mu.Unlock()
	if pending > 0 || s.HeartbeatEvery <= 0 || time.Since(s.lastContact) < s.HeartbeatEvery {
		return
	}
	cancel, err := s.sink.Heartbeat(context.Background())
	if err != nil {
		return
	}
	s.lastContact = time.Now()
	if cancel && s.OnCancel != nil {
		s.OnCancel()
	}
}

// flushOnce sends the oldest batch. Only the shipper goroutine (or Close, after it stopped) calls it.
func (s *Shipper) flushOnce(ctx context.Context) (int, error) {
	s.mu.Lock()
	n := min(len(s.buf), s.batchSize())
	batch := append([]api.LogLine(nil), s.buf[:n]...)
	s.mu.Unlock()
	if n == 0 {
		return 0, nil
	}
	cancel, err := s.sink.SendLogs(ctx, batch)
	if err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return 0, err
	}
	s.mu.Lock()
	s.buf = s.buf[n:]
	s.mu.Unlock()
	s.lastContact = time.Now()
	if cancel && s.OnCancel != nil {
		s.OnCancel()
	}
	return n, nil
}

// Close stops the background loop and sends everything left, until ctx expires.
func (s *Shipper) Close(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		close(s.stop)
		<-s.done
	}
	for {
		s.mu.Lock()
		left := len(s.buf)
		s.mu.Unlock()
		if left == 0 {
			return nil
		}
		if _, err := s.flushOnce(ctx); err != nil {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%d log lines not sent: %w", left, err)
			case <-time.After(s.Backoff):
			}
		}
	}
}
