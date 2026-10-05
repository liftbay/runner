package logship

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/liftbay/runner/internal/api"
)

func TestRedact(t *testing.T) {
	r := &Redactor{}
	r.Add("ab", "s3cret", "s3cret-longer", "-----BEGIN KEY-----\nMIIEabc\n-----END KEY-----")
	cases := map[string]string{
		"token=s3cret-longer!": "token=***!",
		"value s3cret":         "value ***",
		"ab stays":             "ab stays",
		"line MIIEabc":         "line ***",
	}
	for in, want := range cases {
		if got := r.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

type flakySink struct {
	mu      sync.Mutex
	calls   int
	got     []api.LogLine
	batches []int
	cancel  bool
}

func (f *flakySink) SendLogs(_ context.Context, lines []api.LogLine) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls%3 == 0 {
		return false, errors.New("temporary")
	}
	f.got = append(f.got, lines...)
	f.batches = append(f.batches, len(lines))
	return f.cancel, nil
}

func TestShipperKeepsOrderThroughFailures(t *testing.T) {
	sink := &flakySink{}
	s := New(sink, &Redactor{}, nil)
	s.FlushEvery, s.Backoff, s.MaxBatch = 5*time.Millisecond, time.Millisecond, 7
	s.Start()
	for i := range 200 {
		s.Log("p", api.Stdout, strconv.Itoa(i))
		if i%50 == 0 {
			time.Sleep(3 * time.Millisecond)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.got) != 200 {
		t.Fatalf("got %d lines, want 200", len(sink.got))
	}
	for i, l := range sink.got {
		if l.Text != strconv.Itoa(i) {
			t.Fatalf("line %d is %q", i, l.Text)
		}
	}
	for _, n := range sink.batches {
		if n > 7 {
			t.Fatalf("batch of %d exceeds MaxBatch", n)
		}
	}
}

func TestShipperCancelAndRedaction(t *testing.T) {
	sink := &flakySink{cancel: true}
	r := &Redactor{}
	r.Add("hunter2")
	s := New(sink, r, nil)
	cancelled := make(chan struct{}, 1)
	s.OnCancel = func() { cancelled <- struct{}{} }
	s.Log("", api.System, "password hunter2")
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.got[0].Text != "password ***" {
		t.Fatalf("not redacted: %q", sink.got[0].Text)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("OnCancel not called")
	}
}
