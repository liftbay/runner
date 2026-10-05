package recipe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
		path, args, err := signing.WriteKeystore(s, b.Temp)
		if err != nil {
			return err
		}
		b.GradleArgs = append(b.GradleArgs, args...)
		b.androidKey = &androidKey{path: path, alias: s.KeyAlias, password: s.StorePassword}
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
	args = append(args, b.GradleArgs...)
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
		if err := verifySignature(ctx, b, p); err != nil {
			return err
		}
		b.addArtifact("aab", p, ".aab", "application/octet-stream")
	default:
		p, err := newestFile(filepath.Join(outputs, "apk"), ".apk")
		if err != nil {
			return err
		}
		if task != "assembleDebug" {
			if err := verifySignature(ctx, b, p); err != nil {
				return err
			}
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

type androidKey struct{ path, alias, password string }

// verifySignature checks that Gradle really signed the output with the upload key. A build that
// silently falls back to the debug key would otherwise look fine until the Play upload is rejected.
func verifySignature(ctx context.Context, b *Build, output string) error {
	k := b.androidKey
	if k == nil {
		return nil
	}
	var want strings.Builder
	if err := b.runCapture(ctx, "", nil, &want, "keytool", "-list", "-v", "-keystore", k.path, "-storepass", k.password, "-alias", k.alias); err != nil {
		return fmt.Errorf("read upload key fingerprint: %w", err)
	}
	expected := signing.CertSHA256(want.String())

	var got strings.Builder
	var err error
	if strings.HasSuffix(output, ".apk") {
		if tool := apksigner(); tool != "" {
			err = b.runCapture(ctx, "", nil, &got, tool, "verify", "--print-certs", output)
		} else {
			b.logf("apksigner not found: can't check the APK signature")
			return nil
		}
	} else {
		// App bundles are JAR-signed, which keytool can read.
		err = b.runCapture(ctx, "", nil, &got, "keytool", "-printcert", "-jarfile", output)
	}
	if err != nil {
		return fmt.Errorf("read %s signature: %w", filepath.Base(output), err)
	}
	actual := signing.CertSHA256(got.String())
	if expected == "" || actual == "" {
		return fmt.Errorf("could not read the signature fingerprints of %s", filepath.Base(output))
	}
	if actual != expected {
		return fmt.Errorf("%s is signed with a different key (SHA-256 %s) than the upload key (%s)", filepath.Base(output), actual, expected)
	}
	b.logf("Signature verified: signed with upload key %q (SHA-256 %s)", k.alias, expected)
	return nil
}

// apksigner from the newest installed build-tools, or "" when none is installed.
func apksigner() string {
	home := os.Getenv("ANDROID_HOME")
	if home == "" {
		home = os.Getenv("ANDROID_SDK_ROOT")
	}
	if home == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, "build-tools", "*", "apksigner"))
	if len(matches) == 0 {
		return ""
	}
	sort.Slice(matches, func(i, j int) bool {
		return versionLess(filepath.Base(filepath.Dir(matches[i])), filepath.Base(filepath.Dir(matches[j])))
	})
	return matches[len(matches)-1]
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}
