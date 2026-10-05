package proc

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStreamsLines(t *testing.T) {
	var out, errs []string
	err := Exec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo one; echo two >&2; printf three"},
		Stdout: func(l string) { out = append(out, l) }, Stderr: func(l string) { errs = append(errs, l) }})
	if err != nil || len(out) != 2 || out[1] != "three" || len(errs) != 1 || errs[0] != "two" {
		t.Fatalf("out=%v errs=%v err=%v", out, errs, err)
	}
}

func TestCancelKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The child sleep holds stdout open; only killing the group ends the run promptly.
	err := Exec{}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "sleep 30 & sleep 30"}})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}
