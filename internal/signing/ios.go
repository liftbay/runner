package signing

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/liftbay/runner/internal/api"
	"github.com/liftbay/runner/internal/proc"
)

// Shell runs signing commands, logging them without echoing secret arguments.
type Shell struct {
	Run proc.Runner
	Log func(stream, text string)
	// AddSecret registers a value for log redaction.
	AddSecret func(string)
}

func (s Shell) exec(ctx context.Context, captureStdout bool, name string, args ...string) (string, error) {
	var out strings.Builder
	cmd := proc.Cmd{Name: name, Args: args,
		Stdout: func(l string) {
			if captureStdout {
				out.WriteString(l + "\n")
			} else {
				s.Log(api.Stdout, l)
			}
		},
		Stderr: func(l string) { s.Log(api.Stderr, l) }}
	s.Log(api.System, "$ "+cmd.String())
	err := s.Run.Run(ctx, cmd)
	return out.String(), err
}

// IOSInstall is signing material installed for one build. Call Cleanup when the build ends.
type IOSInstall struct {
	Keychain string
	TeamID   string
	// One per bundle id (app and each extension), keyed by bundle id.
	Profiles map[string]InstalledProfile

	sh            Shell
	prevKeychains []string
	paths         []string
}

type InstalledProfile struct{ Name, UUID, BundleID, Path string }

// ProfileNames maps bundle id → profile name, for patching the project and ExportOptions.
func (i *IOSInstall) ProfileNames() map[string]string {
	m := make(map[string]string, len(i.Profiles))
	for id, p := range i.Profiles {
		m[id] = p.Name
	}
	return m
}

// InstallIOS imports the certificate into a temporary keychain and installs every provisioning profile.
func InstallIOS(ctx context.Context, sh Shell, s *api.IOSSigning, dir string) (*IOSInstall, error) {
	inst := &IOSInstall{sh: sh, Keychain: filepath.Join(dir, "liftbay-build.keychain-db"), TeamID: s.TeamID, Profiles: map[string]InstalledProfile{}}
	if len(s.ProvisioningProfilesBase64) == 0 {
		return nil, fmt.Errorf("no provisioning profiles uploaded")
	}
	password := randomHex(16)
	sh.AddSecret(password)
	if s.CertificatePassword != "" {
		sh.AddSecret(s.CertificatePassword)
	}

	p12, err := base64.StdEncoding.DecodeString(s.CertificateP12Base64)
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	p12Path := filepath.Join(dir, "certificate.p12")
	if err := os.WriteFile(p12Path, p12, 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(p12Path)

	if list, err := sh.exec(ctx, true, "security", "list-keychains", "-d", "user"); err == nil {
		for _, l := range strings.Split(list, "\n") {
			if l = strings.Trim(strings.TrimSpace(l), `"`); l != "" {
				inst.prevKeychains = append(inst.prevKeychains, l)
			}
		}
	}
	steps := [][]string{
		{"create-keychain", "-p", password, inst.Keychain},
		{"set-keychain-settings", "-lut", "21600", inst.Keychain},
		{"unlock-keychain", "-p", password, inst.Keychain},
		{"import", p12Path, "-k", inst.Keychain, "-P", s.CertificatePassword, "-f", "pkcs12", "-T", "/usr/bin/codesign", "-T", "/usr/bin/security"},
		{"set-key-partition-list", "-S", "apple-tool:,apple:", "-s", "-k", password, inst.Keychain},
		append([]string{"list-keychains", "-d", "user", "-s", inst.Keychain}, inst.prevKeychains...),
	}
	for _, args := range steps {
		if _, err := sh.exec(ctx, args[0] == "set-key-partition-list", "security", args...); err != nil {
			inst.Cleanup(context.Background())
			return nil, fmt.Errorf("security %s: %w", args[0], err)
		}
	}

	home, _ := os.UserHomeDir()
	profilesDir := filepath.Join(home, "Library", "MobileDevice", "Provisioning Profiles")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		inst.Cleanup(context.Background())
		return nil, err
	}
	for n, b64 := range s.ProvisioningProfilesBase64 {
		if err := inst.installProfile(ctx, b64, n, dir, profilesDir); err != nil {
			inst.Cleanup(context.Background())
			return nil, err
		}
	}
	return inst, nil
}

func (i *IOSInstall) installProfile(ctx context.Context, b64 string, n int, dir, profilesDir string) error {
	profile, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("provisioning profile %d: %w", n+1, err)
	}
	raw := filepath.Join(dir, fmt.Sprintf("profile-%d.mobileprovision", n))
	if err := os.WriteFile(raw, profile, 0o600); err != nil {
		return err
	}
	defer os.Remove(raw)
	xml, err := i.sh.exec(ctx, true, "security", "cms", "-D", "-i", raw)
	if err != nil {
		return fmt.Errorf("decode provisioning profile %d: %w", n+1, err)
	}
	info, err := ReadProfile([]byte(xml))
	if err != nil {
		return fmt.Errorf("provisioning profile %d: %w", n+1, err)
	}
	if err := checkProfile(info, i.TeamID, i.Profiles); err != nil {
		return err
	}
	if i.TeamID == "" {
		i.TeamID = info.TeamID
	}
	path := filepath.Join(profilesDir, info.UUID+".mobileprovision")
	if err := os.WriteFile(path, profile, 0o644); err != nil {
		return err
	}
	i.paths = append(i.paths, path)
	i.Profiles[info.BundleID] = InstalledProfile{Name: info.Name, UUID: info.UUID, BundleID: info.BundleID, Path: path}
	i.sh.Log(api.System, fmt.Sprintf("Installed provisioning profile %q (%s) for %s", info.Name, info.UUID, info.BundleID))
	return nil
}

// checkProfile rejects profiles a distribution build can't use deterministically.
func checkProfile(info ProfileInfo, teamID string, have map[string]InstalledProfile) error {
	switch {
	case info.BundleID == "":
		return fmt.Errorf("provisioning profile %q has no application identifier", info.Name)
	case strings.Contains(info.BundleID, "*"):
		return fmt.Errorf("provisioning profile %q is a wildcard profile (%s); upload an explicit App ID profile for each bundle id", info.Name, info.BundleID)
	case teamID != "" && info.TeamID != "" && info.TeamID != teamID:
		return fmt.Errorf("provisioning profile %q belongs to team %s, not %s", info.Name, info.TeamID, teamID)
	}
	if prev, ok := have[info.BundleID]; ok {
		return fmt.Errorf("two provisioning profiles for %s (%q and %q); upload one per bundle id", info.BundleID, prev.Name, info.Name)
	}
	return nil
}

// Cleanup restores the keychain search list and deletes the keychain and profile.
func (i *IOSInstall) Cleanup(ctx context.Context) {
	if len(i.prevKeychains) > 0 {
		_, _ = i.sh.exec(ctx, true, "security", append([]string{"list-keychains", "-d", "user", "-s"}, i.prevKeychains...)...)
	}
	if _, err := os.Stat(i.Keychain); err == nil {
		_, _ = i.sh.exec(ctx, true, "security", "delete-keychain", i.Keychain)
	}
	for _, p := range i.paths {
		_ = os.Remove(p)
	}
}

type ProfileInfo struct{ Name, UUID, TeamID, BundleID string }

// ReadProfile reads a decoded (XML) provisioning profile.
func ReadProfile(xml []byte) (ProfileInfo, error) {
	v, err := ParsePlist(xml)
	if err != nil {
		return ProfileInfo{}, err
	}
	d, ok := v.(map[string]any)
	if !ok {
		return ProfileInfo{}, fmt.Errorf("provisioning profile is not a dictionary")
	}
	info := ProfileInfo{}
	info.Name, _ = d["Name"].(string)
	info.UUID, _ = d["UUID"].(string)
	if teams, ok := d["TeamIdentifier"].([]any); ok && len(teams) > 0 {
		info.TeamID, _ = teams[0].(string)
	}
	if ent, ok := d["Entitlements"].(map[string]any); ok {
		appID, _ := ent["application-identifier"].(string)
		if _, rest, found := strings.Cut(appID, "."); found {
			info.BundleID = rest
		}
	}
	if info.UUID == "" || info.Name == "" {
		return info, fmt.Errorf("provisioning profile has no UUID or Name")
	}
	return info, nil
}
