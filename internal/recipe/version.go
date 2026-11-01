package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/liftbay/runner/internal/api"
)

// Versioning reads how a profile numbers its builds, like EAS: appVersionSource "remote" (the default: Liftbay keeps
// the build number) or "local" (the app config's number is used as is), and autoIncrement (default: on for store
// builds). liftbay.json wins; eas.json (cli.appVersionSource, build.<profile>.autoIncrement through "extends") is
// read for projects moving from EAS.
func Versioning(dir, profile, distribution string) (remote, increment bool) {
	remote, increment = true, distribution == "store"
	var lb struct {
		AppVersionSource string `json:"appVersionSource"`
		Build            struct {
			Profiles map[string]struct {
				AutoIncrement *bool `json:"autoIncrement"`
			} `json:"profiles"`
		} `json:"build"`
	}
	var eas struct {
		CLI struct {
			AppVersionSource string `json:"appVersionSource"`
		} `json:"cli"`
		Build map[string]struct {
			AutoIncrement json.RawMessage `json:"autoIncrement"`
			Extends       string          `json:"extends"`
		} `json:"build"`
	}
	hasLB := readJSON(filepath.Join(dir, "liftbay.json"), &lb)
	hasEAS := readJSON(filepath.Join(dir, "eas.json"), &eas)

	source := lb.AppVersionSource
	if source == "" && hasEAS {
		source = eas.CLI.AppVersionSource
	}
	remote = source != "local"

	if hasLB && lb.Build.Profiles[profile].AutoIncrement != nil {
		return remote, *lb.Build.Profiles[profile].AutoIncrement
	}
	if hasEAS {
		for name, seen := profile, map[string]bool{}; name != "" && !seen[name]; name = eas.Build[name].Extends {
			seen[name] = true
			if raw := eas.Build[name].AutoIncrement; len(raw) > 0 {
				// true, "buildNumber" and "versionCode" bump the build number; false and "version" (the marketing
				// version, which stays in the app config) don't.
				var b bool
				var s string
				switch {
				case json.Unmarshal(raw, &b) == nil:
					return remote, b
				case json.Unmarshal(raw, &s) == nil:
					return remote, s == "buildNumber" || s == "versionCode"
				}
			}
		}
	}
	return remote, increment
}

func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, v) == nil
}

// versionStep writes the build number (Android versionCode, iOS CFBundleVersion) into the native project, after
// prebuild generated it. Store uploads are rejected when the number was used before, so with remote versioning
// every store build takes the next number Liftbay keeps for the project.
func versionStep() Step {
	return Step{Name: "Set build number", Status: api.StatusRunning, Run: func(ctx context.Context, b *Build) error {
		j := b.Job
		var native nativeVersion
		var err error
		if j.Build.Platform == "ios" {
			native, err = iosVersion(b.Dir)
		} else {
			native, err = androidVersion(b.Dir)
		}
		if err != nil {
			return err
		}
		remote, increment := Versioning(b.Dir, j.Build.Profile, j.Build.Distribution)
		if !remote {
			b.logf("%s %s from the app config (appVersionSource: local)", native.label, orDash(native.local))
			if native.local != "" {
				if _, err := b.API.Update(ctx, api.Update{BuildNumber: native.local}); err != nil {
					b.logf("Could not report the build number: %v", err)
				}
			}
			return nil
		}
		n, err := b.API.BuildNumber(ctx, native.local, increment)
		if err != nil {
			return fmt.Errorf("get the build number from Liftbay: %w", err)
		}
		if err := native.write(n); err != nil {
			return err
		}
		how := "the project's current build number"
		if increment {
			how = "the next build number (autoIncrement)"
		}
		b.logf("%s %s: %s, kept by Liftbay. Change it with `liftbay build:version:set` or in the project settings", native.label, n, how)
		return nil
	}}
}

type nativeVersion struct {
	label string
	local string
	write func(n string) error
}

var gradleVersionCodeRe = regexp.MustCompile(`(?m)^(\s*versionCode\s*=?\s*)(\d+)`)

func androidVersion(dir string) (nativeVersion, error) {
	path := filepath.Join(dir, "android", "app", "build.gradle")
	if _, err := os.Stat(path); err != nil {
		path += ".kts"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nativeVersion{}, errors.New("android/app/build.gradle not found (run expo prebuild, or commit the android project)")
	}
	m := gradleVersionCodeRe.FindSubmatch(data)
	if m == nil {
		return nativeVersion{}, fmt.Errorf("no versionCode in %s", relTo(dir, path))
	}
	return nativeVersion{label: "versionCode", local: string(m[2]), write: func(n string) error {
		return os.WriteFile(path, gradleVersionCodeRe.ReplaceAll(data, []byte("${1}"+n)), 0o644)
	}}, nil
}

var (
	pbxBuildVersionRe    = regexp.MustCompile(`(CURRENT_PROJECT_VERSION = )"?([^";]+)"?;`)
	plistBundleVersionRe = regexp.MustCompile(`(<key>CFBundleVersion</key>\s*<string>)([^<]*)(</string>)`)
)

// iosVersion sets CURRENT_PROJECT_VERSION in the app's Xcode project and CFBundleVersion in every Info.plist that
// has a literal one, so the app and its extensions share the build number (App Store Connect requires that).
func iosVersion(dir string) (nativeVersion, error) {
	projects, _ := filepath.Glob(filepath.Join(dir, "ios", "*.xcodeproj", "project.pbxproj"))
	var pbx []string
	for _, p := range projects {
		if !strings.Contains(p, string(filepath.Separator)+"Pods.xcodeproj") {
			pbx = append(pbx, p)
		}
	}
	var plists []string
	for _, pattern := range []string{filepath.Join(dir, "ios", "*", "Info.plist"), filepath.Join(dir, "ios", "*", "*", "Info.plist")} {
		found, _ := filepath.Glob(pattern)
		for _, p := range found {
			if !strings.Contains(p, string(filepath.Separator)+"Pods"+string(filepath.Separator)) && !strings.Contains(p, ".xcodeproj") {
				plists = append(plists, p)
			}
		}
	}
	if len(pbx) == 0 {
		return nativeVersion{}, errors.New("no Xcode project in ios/ (run expo prebuild, or commit the ios project)")
	}
	local := ""
	for _, p := range pbx {
		if data, err := os.ReadFile(p); err == nil {
			if m := pbxBuildVersionRe.FindSubmatch(data); m != nil {
				local = string(m[2])
				break
			}
		}
	}
	if local == "" || strings.HasPrefix(local, "$") {
		local = ""
		for _, p := range plists {
			if data, err := os.ReadFile(p); err == nil {
				if m := plistBundleVersionRe.FindSubmatch(data); m != nil && !strings.HasPrefix(string(m[2]), "$") {
					local = strings.TrimSpace(string(m[2]))
					break
				}
			}
		}
	}
	return nativeVersion{label: "Build number", local: local, write: func(n string) error {
		for _, p := range pbx {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(p, pbxBuildVersionRe.ReplaceAll(data, []byte("${1}"+n+";")), 0o644); err != nil {
				return err
			}
		}
		for _, p := range plists {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out := plistBundleVersionRe.ReplaceAllFunc(data, func(m []byte) []byte {
				parts := plistBundleVersionRe.FindSubmatch(m)
				if strings.HasPrefix(string(parts[2]), "$") {
					return m // $(CURRENT_PROJECT_VERSION): follows the Xcode project
				}
				return []byte(string(parts[1]) + n + string(parts[3]))
			})
			if err := os.WriteFile(p, out, 0o644); err != nil {
				return err
			}
		}
		return nil
	}}, nil
}

func relTo(dir, path string) string {
	if r, err := filepath.Rel(dir, path); err == nil {
		return r
	}
	return path
}
