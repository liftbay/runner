package recipe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersioningDefaultsAndConfig(t *testing.T) {
	dir := t.TempDir()
	if remote, inc := Versioning(dir, "production", "store"); !remote || !inc {
		t.Fatalf("store default: remote %v increment %v", remote, inc)
	}
	if remote, inc := Versioning(dir, "preview", "internal"); !remote || inc {
		t.Fatalf("internal default: remote %v increment %v", remote, inc)
	}

	// eas.json, followed through "extends", for projects moving from EAS.
	_ = os.WriteFile(filepath.Join(dir, "eas.json"), []byte(`{"cli":{"appVersionSource":"remote"},"build":{"base":{"autoIncrement":true},"preview":{"extends":"base"},"production":{"autoIncrement":"version"}}}`), 0o644)
	if _, inc := Versioning(dir, "preview", "internal"); !inc {
		t.Fatal("eas.json autoIncrement through extends")
	}
	if _, inc := Versioning(dir, "production", "store"); inc {
		t.Fatal(`autoIncrement "version" bumps the marketing version, not the build number`)
	}

	// liftbay.json wins.
	_ = os.WriteFile(filepath.Join(dir, "liftbay.json"), []byte(`{"appVersionSource":"local","build":{"profiles":{"production":{"autoIncrement":true}}}}`), 0o644)
	if remote, inc := Versioning(dir, "production", "store"); remote || !inc {
		t.Fatalf("liftbay.json: remote %v increment %v", remote, inc)
	}
}

func TestIOSVersionWritesProjectAndPlists(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(s), 0o644)
	}
	write("ios/App.xcodeproj/project.pbxproj", "a\n\t\t\t\tCURRENT_PROJECT_VERSION = 1;\nb\n\t\t\t\tCURRENT_PROJECT_VERSION = 1;\n")
	write("ios/Pods/Pods.xcodeproj/project.pbxproj", "CURRENT_PROJECT_VERSION = 1;\n")
	write("ios/App/Info.plist", "<key>CFBundleVersion</key>\n\t<string>$(CURRENT_PROJECT_VERSION)</string>")
	write("ios/Widget/Info.plist", "<key>CFBundleVersion</key>\n\t<string>1</string>")

	v, err := iosVersion(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v.local != "1" {
		t.Fatalf("local %q", v.local)
	}
	if err := v.write("17"); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string { b, _ := os.ReadFile(filepath.Join(dir, rel)); return string(b) }
	if got := read("ios/App.xcodeproj/project.pbxproj"); strings.Count(got, "CURRENT_PROJECT_VERSION = 17;") != 2 {
		t.Fatalf("pbxproj %s", got)
	}
	if got := read("ios/Pods/Pods.xcodeproj/project.pbxproj"); !strings.Contains(got, "= 1;") {
		t.Fatalf("Pods project changed: %s", got)
	}
	if got := read("ios/App/Info.plist"); !strings.Contains(got, "$(CURRENT_PROJECT_VERSION)") {
		t.Fatalf("app plist %s", got)
	}
	if got := read("ios/Widget/Info.plist"); !strings.Contains(got, "<string>17</string>") {
		t.Fatalf("extension plist %s", got)
	}
}
