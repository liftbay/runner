package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/liftbay/runner/internal/api"
	"github.com/liftbay/runner/internal/signing"
)

type iosState struct {
	workspace, scheme string
	install           *signing.IOSInstall
	archive           string
}

func iosSteps(job *api.Job) []Step {
	steps := []Step{setupStep(), installStep()}
	if job.Project.Framework == "expo" {
		steps = append(steps, prebuildStep("ios"))
	}
	steps = append(steps, Step{Name: "Pod install", Status: api.StatusRunning, Run: podInstall})
	signed := job.Build.Distribution != "simulator" && job.Signing.IOS != nil
	if signed {
		steps = append(steps,
			Step{Name: "Prepare signing", Status: api.StatusRunning, Run: iosSigning},
			Step{Name: "Archive", Status: api.StatusRunning, Run: archive},
			Step{Name: "Export IPA", Status: api.StatusRunning, Run: exportIPA},
		)
	} else {
		steps = append(steps, Step{Name: "Build for simulator", Status: api.StatusRunning, Run: simulatorBuild})
	}
	return append(steps, uploadStep())
}

func podInstall(ctx context.Context, b *Build) error {
	if b.Job.Build.Distribution != "simulator" && b.Job.Signing.IOS == nil {
		b.logf("No iOS signing credentials for this project: building for the iOS simulator instead of an .ipa")
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "ios", "Podfile")); err != nil {
		return errors.New("ios/Podfile not found (run expo prebuild, or commit the ios project)")
	}
	if err := b.run(ctx, "ios", []string{"LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8"}, "pod", "install"); err != nil {
		return err
	}
	return detectXcodeTarget(ctx, b)
}

func detectXcodeTarget(ctx context.Context, b *Build) error {
	matches, _ := filepath.Glob(filepath.Join(b.Dir, "ios", "*.xcworkspace"))
	if len(matches) == 0 {
		return errors.New("no .xcworkspace in ios/")
	}
	b.ios.workspace = matches[0]
	var out strings.Builder
	if err := b.runCapture(ctx, "ios", nil, &out, "xcodebuild", "-list", "-json", "-workspace", b.ios.workspace); err != nil {
		return err
	}
	var list struct {
		Workspace struct {
			Schemes []string `json:"schemes"`
		} `json:"workspace"`
	}
	// xcodebuild may print warnings before the JSON.
	raw := out.String()
	if i := strings.Index(raw, "{"); i >= 0 {
		raw = raw[i:]
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return fmt.Errorf("read schemes: %w", err)
	}
	name := strings.TrimSuffix(filepath.Base(b.ios.workspace), ".xcworkspace")
	b.ios.scheme = ChooseScheme(name, list.Workspace.Schemes)
	if b.ios.scheme == "" {
		return errors.New("no app scheme found in the workspace")
	}
	b.logf("Workspace %s, scheme %s", filepath.Base(b.ios.workspace), b.ios.scheme)
	return nil
}

// ChooseScheme prefers the scheme named after the workspace, then the first that isn't a Pods or
// React Native library scheme.
func ChooseScheme(workspace string, schemes []string) string {
	for _, s := range schemes {
		if s == workspace {
			return s
		}
	}
	lib := []string{"Pods", "React", "RCT", "Yoga", "hermes", "Expo", "EX", "boost", "glog", "fmt", "DoubleConversion", "RNC", "FBReact", "FBLazy", "SocketRocket", "lottie"}
outer:
	for _, s := range schemes {
		for _, p := range lib {
			if strings.HasPrefix(s, p) {
				continue outer
			}
		}
		return s
	}
	return ""
}

func iosSigning(ctx context.Context, b *Build) error {
	sh := signing.Shell{Run: b.Proc, Log: func(stream, text string) { b.Log.Log(b.phase, stream, text) }, AddSecret: b.AddSecret}
	inst, err := signing.InstallIOS(ctx, sh, b.Job.Signing.IOS, b.Temp)
	if err != nil {
		return err
	}
	b.ios.install = inst
	b.cleanups = append(b.cleanups, func() { inst.Cleanup(context.Background()) })

	profiles := inst.ProfileNames()
	if id := b.Job.Project.BundleID; id != "" {
		if _, ok := profiles[id]; !ok {
			return fmt.Errorf("no provisioning profile for the app's bundle id %s; upload a profile for it", id)
		}
	}
	projects, _ := filepath.Glob(filepath.Join(b.Dir, "ios", "*.xcodeproj", "project.pbxproj"))
	var patched []signing.Patched
	var missing []string
	for _, p := range projects {
		if strings.Contains(p, "Pods.xcodeproj") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// Every app and extension target must be covered, or the archive fails late with a vague error.
		missing = append(missing, signing.MissingProfiles(signing.SignedTargets(string(src)), profiles)...)
		out, done := signing.PatchPbxproj(string(src), inst.TeamID, profiles)
		if len(done) > 0 {
			if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
				return err
			}
			patched = append(patched, done...)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("upload a provisioning profile for: %s", strings.Join(missing, ", "))
	}
	if len(patched) == 0 {
		return errors.New("no app target in the Xcode project matches the uploaded provisioning profiles")
	}
	for _, p := range patched {
		b.logf("%s (%s): %s → profile %q", p.Target, p.Config, p.BundleID, p.Profile)
	}
	return nil
}

func archive(ctx context.Context, b *Build) error {
	b.ios.archive = filepath.Join(b.Temp, "App.xcarchive")
	return b.run(ctx, "ios", nil, "xcodebuild",
		"-workspace", b.ios.workspace, "-scheme", b.ios.scheme, "-configuration", "Release",
		"-destination", "generic/platform=iOS", "-archivePath", b.ios.archive,
		"-hideShellScriptEnvironment", "archive",
		"OTHER_CODE_SIGN_FLAGS=--keychain "+b.ios.install.Keychain)
}

func exportIPA(ctx context.Context, b *Build) error {
	inst := b.ios.install
	method := b.Job.Signing.IOS.ExportMethod
	if method == "" {
		method = "app-store"
	}
	plist := filepath.Join(b.Temp, "ExportOptions.plist")
	if err := os.WriteFile(plist, []byte(signing.ExportOptions(method, inst.TeamID, inst.ProfileNames())), 0o644); err != nil {
		return err
	}
	out := filepath.Join(b.Temp, "export")
	if err := b.run(ctx, "ios", nil, "xcodebuild", "-exportArchive", "-archivePath", b.ios.archive, "-exportPath", out, "-exportOptionsPlist", plist); err != nil {
		return err
	}
	ipa, err := newestFile(out, ".ipa")
	if err != nil {
		return err
	}
	b.addArtifact("ipa", ipa, ".ipa", "application/octet-stream")
	if dsyms := filepath.Join(b.ios.archive, "dSYMs"); dirHasEntries(dsyms) {
		zip := filepath.Join(b.Temp, "dSYMs.zip")
		if err := b.run(ctx, "", nil, "ditto", "-c", "-k", "--keepParent", dsyms, zip); err == nil {
			b.addArtifact("dsym", zip, "-dSYMs.zip", "application/zip")
		}
	}
	return nil
}

func simulatorBuild(ctx context.Context, b *Build) error {
	derived := filepath.Join(b.Temp, "DerivedData")
	if err := b.run(ctx, "ios", nil, "xcodebuild",
		"-workspace", b.ios.workspace, "-scheme", b.ios.scheme, "-configuration", "Release",
		"-sdk", "iphonesimulator", "-destination", "generic/platform=iOS Simulator", "-derivedDataPath", derived,
		"-hideShellScriptEnvironment", "CODE_SIGNING_ALLOWED=NO", "build"); err != nil {
		return err
	}
	apps, _ := filepath.Glob(filepath.Join(derived, "Build", "Products", "Release-iphonesimulator", "*.app"))
	if len(apps) == 0 {
		return errors.New("no .app found in the build products")
	}
	zip := filepath.Join(b.Temp, "App.app.zip")
	if err := b.run(ctx, "", nil, "ditto", "-c", "-k", "--keepParent", apps[0], zip); err != nil {
		return err
	}
	b.addArtifact("app", zip, "-simulator.app.zip", "application/zip")
	return nil
}

func dirHasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}
