package recipe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/liftbay/runner/internal/api"
	"github.com/liftbay/runner/internal/proc"
	"github.com/liftbay/runner/internal/signing"
)

func androidSteps(job *api.Job) []Step {
	steps := []Step{setupStep(), installStep()}
	if job.Project.Framework == "expo" {
		steps = append(steps, prebuildStep("android"))
	}
	if job.Build.Distribution != "simulator" {
		steps = append(steps, androidSigningStep())
	}
	task := gradleTask(job.Build.Distribution)
	steps = append(steps, Step{Name: "Gradle " + task, Status: api.StatusRunning, Run: func(ctx context.Context, b *Build) error {
		return gradle(ctx, b, task)
	}}, uploadStep())
	return steps
}

func gradleTask(distribution string) string {
	switch distribution {
	case "internal":
		return "assembleRelease"
	case "simulator":
		return "assembleDebug"
	default:
		return "bundleRelease"
	}
}

func androidSigningStep() Step {
	return Step{Name: "Prepare signing", Status: api.StatusRunning, Run: func(ctx context.Context, b *Build) error {
		s := b.Job.Signing.Android
		if s == nil {
			b.logf("No upload key for this project yet: generating one")
			gen, err := signing.GenerateUploadKeystore(ctx, b.Proc, func(proc.Cmd) { b.logf("$ keytool -genkeypair … (passwords hidden)") }, b.Temp, b.Job.Project.Name)
			if err != nil {
				return fmt.Errorf("generate upload key: %w", err)
			}
			b.AddSecret(gen.StorePassword)
			switch err := b.API.PutAndroidSigning(ctx, *gen); {
			case err == nil:
				b.logf("Upload key stored (encrypted) with the project")
				s = gen
			case api.IsStatus(err, 409):
				// Another build stored one first; use that so every build signs with the same key.
				job, err := b.API.Job(ctx)
				if err != nil {
					return err
				}
				if job.Signing.Android == nil {
					return errors.New("the project already has an upload key, but it was not returned")
				}
				s = job.Signing.Android
			default:
				return fmt.Errorf("store upload key: %w", err)
			}
		}
		b.AddSecret(s.StorePassword)
		b.AddSecret(s.KeyPassword)
		props, err := signing.WriteKeystore(s, b.Temp)
		if err != nil {
			return err
		}
		b.Env = append(b.Env, props...)
		b.logf("Signing with upload key %q", s.KeyAlias)
		return nil
	}}
}

var xmxRe = regexp.MustCompile(`(?m)^\s*org\.gradle\.jvmargs\s*=.*-Xmx(\d+)([gGmM])`)

// gradleHeapTooSmall reports whether gradle.properties leaves less than 3 GB for Gradle. Expo's
// template default (2 GB) runs out of memory in dex merging on release builds.
func gradleHeapTooSmall(props string) bool {
	m := xmxRe.FindStringSubmatch(props)
	if m == nil {
		return true
	}
	n, _ := strconv.Atoi(m[1])
	if strings.EqualFold(m[2], "m") {
		return n < 3072
	}
	return n < 3
}

func gradle(ctx context.Context, b *Build, task string) error {
	androidDir := filepath.Join(b.Dir, "android")
	gradlew := filepath.Join(androidDir, "gradlew")
	if _, err := os.Stat(gradlew); err != nil {
		return errors.New("android/gradlew not found (run expo prebuild, or commit the android project)")
	}
	_ = os.Chmod(gradlew, 0o755)
	args := []string{task, "--no-daemon", "--console=plain", "--stacktrace"}
	props, _ := os.ReadFile(filepath.Join(androidDir, "gradle.properties"))
	if gradleHeapTooSmall(string(props)) {
		b.logf("Raising the Gradle heap to 4 GB for this build")
		args = append(args, "-Dorg.gradle.jvmargs=-Xmx4g -XX:MaxMetaspaceSize=1g")
	}
	if err := b.run(ctx, "android", nil, "./gradlew", args...); err != nil {
		return err
	}
	outputs := filepath.Join(androidDir, "app", "build", "outputs")
	switch task {
	case "bundleRelease":
		p, err := newestFile(filepath.Join(outputs, "bundle"), ".aab")
		if err != nil {
			return err
		}
		b.addArtifact("aab", p, ".aab", "application/octet-stream")
	default:
		p, err := newestFile(filepath.Join(outputs, "apk"), ".apk")
		if err != nil {
			return err
		}
		b.addArtifact("apk", p, ".apk", "application/vnd.android.package-archive")
	}
	if mapping := filepath.Join(outputs, "mapping", "release", "mapping.txt"); task != "assembleDebug" {
		if _, err := os.Stat(mapping); err == nil {
			b.addArtifact("mapping", mapping, "-mapping.txt", "text/plain")
		}
	}
	return nil
}

// newestFile finds the most recently written file with ext under root.
func newestFile(root, ext string) (string, error) {
	var best string
	var bestMod int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ext) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().UnixNano() > bestMod {
			best, bestMod = p, info.ModTime().UnixNano()
		}
		return nil
	})
	if best == "" {
		return "", fmt.Errorf("no %s file found under %s", ext, root)
	}
	return best, nil
}
