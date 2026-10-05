// Package proc runs build commands in their own process group and streams their output by line.
package proc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Cmd struct {
	Name string
	Args []string
	Dir  string
	// Full environment; nil inherits the runner's.
	Env    []string
	Stdout func(line string)
	Stderr func(line string)
}

// String renders the command for the "$ …" log line.
func (c Cmd) String() string {
	parts := []string{c.Name}
	for _, a := range c.Args {
		if a == "" || strings.ContainsAny(a, " \t\"'$") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

type Runner interface {
	Run(ctx context.Context, c Cmd) error
}

// Exec runs real processes. Cancelling ctx kills the whole process group (Gradle and xcodebuild
// spawn children that would otherwise keep running).
type Exec struct{}

func (Exec) Run(ctx context.Context, c Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		go func(pid int) {
			time.Sleep(10 * time.Second)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}(cmd.Process.Pid)
		return nil
	}
	cmd.WaitDelay = 15 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scanLines(stdout, c.Stdout) }()
	go func() { defer wg.Done(); scanLines(stderr, c.Stderr) }()
	wg.Wait()
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func scanLines(r io.Reader, fn func(string)) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" && fn != nil {
			fn(strings.TrimRight(line, "\r\n"))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && fn != nil {
				fn("(output error: " + err.Error() + ")")
			}
			return
		}
	}
}
