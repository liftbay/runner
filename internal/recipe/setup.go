package recipe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/liftbay/runner/internal/api"
)

type PackageManager struct {
	Name    string
	Install []string
}

// DetectPackageManager picks the install command from the lockfile in dir.
func DetectPackageManager(dir string) PackageManager {
	has := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	switch {
	case has("bun.lock") || has("bun.lockb"):
		return PackageManager{"bun", []string{"bun", "install", "--frozen-lockfile"}}
	case has("pnpm-lock.yaml"):
		return PackageManager{"pnpm", []string{"pnpm", "install", "--frozen-lockfile"}}
	case has("yarn.lock") && has(".yarnrc.yml"):
		return PackageManager{"yarn", []string{"yarn", "install", "--immutable"}}
	case has("yarn.lock"):
		return PackageManager{"yarn", []string{"yarn", "install", "--frozen-lockfile"}}
	case has("package-lock.json"):
		return PackageManager{"npm", []string{"npm", "ci", "--no-audit", "--no-fund"}}
	default:
		return PackageManager{"npm", []string{"npm", "install", "--no-audit", "--no-fund"}}
	}
}

func setupStep() Step {
	return Step{Name: "Set up environment", Status: api.StatusPreparing, Run: func(ctx context.Context, b *Build) error {
		j := b.Job
		b.logf("Build #%d · %s · %s · profile %s · environment %s · %s", j.Build.Number, j.Project.Name, j.Build.Platform, j.Build.Profile, j.Build.Environment, j.Build.Distribution)
		keys := make([]string, 0, len(j.Env))
		for _, v := range j.Env {
			keys = append(keys, v.Key)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			b.logf("Environment variables: %s", strings.Join(keys, ", "))
		}
		if err := writeDotEnv(filepath.Join(b.Dir, ".env"), j.Env); err != nil {
			return err
		}
		meta := readMetadata(ctx, b.Dir, j.Build.Platform)
		if meta.Commit != "" || meta.AppVersion != "" || meta.BuildNumber != "" {
			b.logf("Commit %s · version %s (%s)", orDash(meta.Commit), orDash(meta.AppVersion), orDash(meta.BuildNumber))
			if _, err := b.API.Update(ctx, meta); err != nil {
				b.logf("Could not report build metadata: %v", err)
			}
		}
		return nil
	}}
}

// writeDotEnv adds the job's variables to .env. They are also exported into every command's
// process environment, which takes precedence for Expo and Gradle; the file is for tools that
// only read .env (e.g. react-native-config). Keys already in the file keep the file's value
// but the process environment still wins for dotenv-based loaders. The runner is ephemeral.
func writeDotEnv(path string, env []api.EnvVar) error {
	existing := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export "))
			if k, _, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
				existing[strings.TrimSpace(k)] = true
			}
		}
		f.Close()
	}
	var b strings.Builder
	for _, v := range env {
		if existing[v.Key] {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", v.Key, strconv.Quote(v.Value))
	}
	if b.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n# Added by Liftbay for this build\n" + b.String())
	return err
}

func installStep() Step {
	return Step{Name: "Install dependencies", Status: api.StatusRunning, Run: func(ctx context.Context, b *Build) error {
		pm := DetectPackageManager(b.Dir)
		if pm.Name == "pnpm" || pm.Name == "yarn" {
			if _, err := exec.LookPath(pm.Name); err != nil {
				if err := b.run(ctx, "", nil, "corepack", "enable"); err != nil {
					return err
				}
			}
		}
		return b.run(ctx, "", nil, pm.Install[0], pm.Install[1:]...)
	}}
}

func prebuildStep(platform string) Step {
	return Step{Name: "Prebuild", Status: api.StatusRunning, Run: func(ctx context.Context, b *Build) error {
		return b.run(ctx, "", nil, "npx", "expo", "prebuild", "--platform", platform, "--no-install")
	}}
}

func readMetadata(ctx context.Context, dir, platform string) api.Update {
	var u api.Update
	u.Commit = os.Getenv("GITHUB_SHA")
	if u.Commit == "" {
		if out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
			u.Commit = strings.TrimSpace(string(out))
		}
	}
	var app struct {
		Expo struct {
			Version string `json:"version"`
			IOS     struct {
				BuildNumber string `json:"buildNumber"`
			} `json:"ios"`
			Android struct {
				VersionCode json.Number `json:"versionCode"`
			} `json:"android"`
		} `json:"expo"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, "app.json")); err == nil && json.Unmarshal(data, &app) == nil {
		u.AppVersion = app.Expo.Version
		if platform == "ios" {
			u.BuildNumber = app.Expo.IOS.BuildNumber
		} else {
			u.BuildNumber = app.Expo.Android.VersionCode.String()
		}
	}
	if platform == "android" && (u.AppVersion == "" || u.BuildNumber == "") {
		if data, err := os.ReadFile(filepath.Join(dir, "android", "app", "build.gradle")); err == nil {
			name, code := gradleVersion(string(data))
			if u.AppVersion == "" {
				u.AppVersion = name
			}
			if u.BuildNumber == "" {
				u.BuildNumber = code
			}
		}
	}
	return u
}

var (
	versionNameRe = regexp.MustCompile(`(?m)^\s*versionName\s*=?\s*"([^"]+)"`)
	versionCodeRe = regexp.MustCompile(`(?m)^\s*versionCode\s*=?\s*(\d+)`)
)

func gradleVersion(src string) (name, code string) {
	if m := versionNameRe.FindStringSubmatch(src); m != nil {
		name = m[1]
	}
	if m := versionCodeRe.FindStringSubmatch(src); m != nil {
		code = m[1]
	}
	return
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}
