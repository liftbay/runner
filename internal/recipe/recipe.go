// Package recipe turns a job into build steps and runs them, reporting phases to the API.
package recipe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liftbay/runner/internal/api"
	"github.com/liftbay/runner/internal/proc"
)

// API is the part of the runner protocol recipes use.
type API interface {
	Update(ctx context.Context, u api.Update) (bool, error)
	Job(ctx context.Context) (*api.Job, error)
	PutAndroidSigning(ctx context.Context, s api.AndroidSigning) error
	UploadArtifact(ctx context.Context, kind, name, path, contentType string) error
}

type Logger interface {
	Log(phase, stream, text string)
}

// Build is the state shared by the steps of one build.
type Build struct {
	Job *api.Job
	// Repository root.
	Dir string
	// Scratch space outside the repository (keystores, archives, keychains).
	Temp string
	API  API
	Proc proc.Runner
	Log  Logger
	// Env is the full process environment for every command (runner env + job env).
	Env []string
	// AddSecret registers a value generated during the build for log redaction.
	AddSecret func(string)
	// OnCancel is called when the API reports a cancel request.
	OnCancel func()

	phase     string
	artifacts []artifact
	cleanups  []func()
	ios       iosState
}

type artifact struct{ kind, name, path, contentType string }

type Step struct {
	Name string
	// Build status reported while this step runs.
	Status string
	Run    func(ctx context.Context, b *Build) error
}

var ErrUnsupported = errors.New("not supported by this runner yet")

// Steps returns the recipe for the job's framework and platform.
func Steps(job *api.Job) ([]Step, error) {
	fw := job.Project.Framework
	if fw != "expo" && fw != "react-native" {
		return nil, fmt.Errorf("framework %q is %w (supported: expo, react-native)", fw, ErrUnsupported)
	}
	switch job.Build.Platform {
	case "android":
		return androidSteps(job), nil
	case "ios":
		return iosSteps(job), nil
	default:
		return nil, fmt.Errorf("platform %q is %w", job.Build.Platform, ErrUnsupported)
	}
}

// Execute runs the steps in order and returns the final build status.
func Execute(ctx context.Context, b *Build, steps []Step) string {
	defer func() {
		for i := len(b.cleanups) - 1; i >= 0; i-- {
			b.cleanups[i]()
		}
	}()
	phases := make([]api.Phase, len(steps))
	for i, s := range steps {
		phases[i] = api.Phase{Name: s.Name, Status: api.StatusQueued}
	}
	report := func(status string) {
		cancel, err := b.API.Update(context.WithoutCancel(ctx), api.Update{Status: status, Phases: phases})
		if err != nil {
			b.Log.Log(b.phase, api.System, "Could not report progress: "+err.Error())
		}
		if cancel && b.OnCancel != nil {
			b.OnCancel()
		}
	}
	for i, s := range steps {
		b.phase = s.Name
		if ctx.Err() != nil {
			phases[i].Status = api.StatusCancelled
			report("")
			return api.StatusCancelled
		}
		phases[i].Status = api.StatusRunning
		report(s.Status)
		started := time.Now()
		err := s.Run(ctx, b)
		d := int(time.Since(started).Seconds())
		phases[i].DurationSec = &d
		switch {
		case ctx.Err() != nil:
			phases[i].Status = api.StatusCancelled
			b.Log.Log(s.Name, api.System, "Build cancelled")
			report("")
			return api.StatusCancelled
		case err != nil:
			phases[i].Status = api.StatusFailed
			b.Log.Log(s.Name, api.Stderr, fmt.Sprintf("%s failed: %v", s.Name, err))
			report("")
			return api.StatusFailed
		}
		phases[i].Status = api.StatusSucceeded
		report("")
	}
	return api.StatusSucceeded
}

// run executes a command in dir (relative to the repo root), streaming its output.
func (b *Build) run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) error {
	return b.runCapture(ctx, dir, extraEnv, nil, name, args...)
}

// runCapture is run, but stdout goes to capture instead of the log when capture is set.
func (b *Build) runCapture(ctx context.Context, dir string, extraEnv []string, capture *strings.Builder, name string, args ...string) error {
	phase := b.phase
	cmd := proc.Cmd{Name: name, Args: args, Dir: filepath.Join(b.Dir, dir), Env: append(append([]string{}, b.Env...), extraEnv...),
		Stdout: func(l string) {
			if capture != nil {
				capture.WriteString(l + "\n")
				return
			}
			b.Log.Log(phase, api.Stdout, l)
		},
		Stderr: func(l string) { b.Log.Log(phase, api.Stderr, l) }}
	b.Log.Log(phase, api.System, "$ "+cmd.String())
	return b.Proc.Run(ctx, cmd)
}

func (b *Build) logf(format string, args ...any) {
	b.Log.Log(b.phase, api.System, fmt.Sprintf(format, args...))
}

func (b *Build) addArtifact(kind, path, suffix, contentType string) {
	name := fmt.Sprintf("%s-%d%s", b.Job.Project.Slug, b.Job.Build.Number, suffix)
	b.artifacts = append(b.artifacts, artifact{kind: kind, name: name, path: path, contentType: contentType})
}

func uploadStep() Step {
	return Step{Name: "Upload artifacts", Status: api.StatusUploading, Run: func(ctx context.Context, b *Build) error {
		if len(b.artifacts) == 0 {
			return errors.New("the build produced no artifacts")
		}
		for _, a := range b.artifacts {
			info, err := os.Stat(a.path)
			if err != nil {
				return err
			}
			b.logf("Uploading %s (%.1f MB)", a.name, float64(info.Size())/1e6)
			if err := b.API.UploadArtifact(ctx, a.kind, a.name, a.path, a.contentType); err != nil {
				return fmt.Errorf("upload %s: %w", a.name, err)
			}
			b.logf("Uploaded %s", a.name)
		}
		return nil
	}}
}
