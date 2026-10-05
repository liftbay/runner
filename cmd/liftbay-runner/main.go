// Command liftbay-runner runs one Liftbay build on the current machine.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/liftbay/runner/internal/api"
	"github.com/liftbay/runner/internal/logship"
	"github.com/liftbay/runner/internal/proc"
	"github.com/liftbay/runner/internal/recipe"
)

// Set with -ldflags "-X main.version=… -X main.commit=…".
var (
	version = "dev"
	commit  = "unknown"
)

const defaultAPI = "https://api.liftbay.dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Printf("liftbay-runner %s (commit %s, sha256 %s, %s/%s)\n", version, commit, selfHash(), runtime.GOOS, runtime.GOARCH)
	case "run":
		os.Exit(run(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  liftbay-runner run [--build-id ID] [--api URL] [--workdir DIR]
  liftbay-runner version

Environment: LIFTBAY_BUILD_ID, LIFTBAY_API_URL, LIFTBAY_JOB_TOKEN (otherwise a GitHub OIDC
token is exchanged for one).`)
}

func run(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	buildID := fs.String("build-id", os.Getenv("LIFTBAY_BUILD_ID"), "build to run")
	apiURL := fs.String("api", envOr("LIFTBAY_API_URL", defaultAPI), "Liftbay API URL")
	workdir := fs.String("workdir", ".", "repository root")
	_ = fs.Parse(args)
	if *buildID == "" {
		fmt.Fprintln(os.Stderr, "liftbay-runner: --build-id or LIFTBAY_BUILD_ID is required")
		return 2
	}
	dir, err := filepath.Abs(*workdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "liftbay-runner:", err)
		return 2
	}

	// SIGTERM / SIGINT (GitHub cancelling the run) ends the build as cancelled.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancelBuild := context.WithCancel(sigCtx)
	defer cancelBuild()

	onGitHub := os.Getenv("GITHUB_ACTIONS") == "true"
	client := api.New(*apiURL, *buildID, os.Getenv("LIFTBAY_JOB_TOKEN"))
	if client.Token == "" {
		if !api.GitHubOIDCAvailable() {
			fmt.Fprintln(os.Stderr, "liftbay-runner: set LIFTBAY_JOB_TOKEN, or run in GitHub Actions with `permissions: id-token: write`")
			return 2
		}
		oidc, err := api.GitHubOIDCToken(ctx)
		if err == nil {
			client.Token, err = client.Exchange(ctx, oidc)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "liftbay-runner: could not get a job token:", err)
			return 1
		}
	}
	if onGitHub {
		fmt.Printf("::add-mask::%s\n", client.Token)
	}

	redactor := &logship.Redactor{}
	redactor.Add(client.Token)
	shipper := logship.New(client, redactor, os.Stdout)
	shipper.OnCancel = cancelBuild
	shipper.Start()
	sys := func(format string, a ...any) { shipper.Log("", api.System, fmt.Sprintf(format, a...)) }

	sys("liftbay-runner %s (commit %s, %s/%s)", version, commit, runtime.GOOS, runtime.GOARCH)
	sys("runner sha256 %s", selfHash())

	status := build(ctx, client, shipper, redactor, dir, onGitHub, sys)

	done, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := shipper.Close(done); err != nil {
		fmt.Fprintln(os.Stderr, "liftbay-runner:", err)
	}
	if err := client.Finish(done, status); err != nil {
		fmt.Fprintln(os.Stderr, "liftbay-runner: could not finish the build:", err)
		return 1
	}
	fmt.Printf("Build %s\n", status)
	if status != api.StatusSucceeded {
		return 1
	}
	return 0
}

func build(ctx context.Context, client *api.Client, shipper *logship.Shipper, redactor *logship.Redactor, dir string, onGitHub bool, sys func(string, ...any)) string {
	job, err := client.Job(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return api.StatusCancelled
		}
		sys("Could not load the job: %v", err)
		return api.StatusFailed
	}
	addSecret := func(v string) {
		redactor.Add(v)
		if onGitHub && len(v) >= 3 {
			for _, line := range splitLines(v) {
				fmt.Printf("::add-mask::%s\n", line)
			}
		}
	}
	env := append(os.Environ(), "CI=1")
	for _, v := range job.Env {
		if v.Secret {
			addSecret(v.Value)
		}
		env = append(env, v.Key+"="+v.Value)
	}

	steps, err := recipe.Steps(job)
	if err != nil {
		sys("%v", err)
		return api.StatusFailed
	}
	temp, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "liftbay-")
	if err != nil {
		sys("%v", err)
		return api.StatusFailed
	}
	defer os.RemoveAll(temp)

	b := &recipe.Build{
		Job: job, Dir: dir, Temp: temp, API: client, Proc: proc.Exec{}, Log: shipper,
		Env: env, AddSecret: addSecret, OnCancel: shipper.OnCancel,
	}
	return recipe.Execute(ctx, b, steps)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if line := s[start:i]; len(line) >= 3 {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}

func selfHash() string {
	path, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	f, err := os.Open(path)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(h.Sum(nil))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
