package signing

import (
	"regexp"
	"sort"
	"strings"
)

// A project.pbxproj is an old-style plist. Objects sit one per block:
//
//	13B07F861A680F5B00A75B9A /* App */ = {
//		isa = PBXNativeTarget;
//		...
//	};
//
// We only read and rewrite those blocks line by line, so the file keeps its exact formatting.

var (
	blockStart  = regexp.MustCompile(`^\s*([0-9A-Fa-f]{24}) /\* .* \*/ = \{$`)
	signingKeys = regexp.MustCompile(`^\s*"?(CODE_SIGN_STYLE|DEVELOPMENT_TEAM|PROVISIONING_PROFILE_SPECIFIER|PROVISIONING_PROFILE|CODE_SIGN_IDENTITY(\[sdk=iphoneos\*\])?)"? = `)
	settingLine = regexp.MustCompile(`^\s*"?([A-Za-z0-9_\[\]=*]+)"? = (.*);$`)
	idRef       = regexp.MustCompile(`([0-9A-Fa-f]{24})`)
	varRef      = regexp.MustCompile(`\$[({]([A-Za-z0-9_]+)(?::[^)}]*)?[)}]`)
)

// Product types that are signed with their own provisioning profile.
var signedProducts = map[string]bool{
	"com.apple.product-type.application":                     true,
	"com.apple.product-type.app-extension":                   true,
	"com.apple.product-type.extensionkit-extension":          true,
	"com.apple.product-type.application.watchapp2":           true,
	"com.apple.product-type.watchkit2-extension":             true,
	"com.apple.product-type.app-extension.messages":          true,
	"com.apple.product-type.application.messages":            true,
	"com.apple.product-type.application.watchapp2-container": true,
}

type block struct {
	id         string
	start, end int // line indexes, inclusive
	lines      []string
}

func (b block) value(key string) string {
	for _, l := range b.lines {
		if m := settingLine.FindStringSubmatch(l); m != nil && m[1] == key {
			return unquote(m[2])
		}
	}
	return ""
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = strings.ReplaceAll(v[1:len(v)-1], `\"`, `"`)
	}
	return v
}

func parseBlocks(lines []string) []block {
	var out []block
	for i := 0; i < len(lines); i++ {
		m := blockStart.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
		end := i + 1
		for end < len(lines) && strings.TrimRight(lines[end], "\r") != indent+"};" {
			end++
		}
		if end >= len(lines) {
			break
		}
		out = append(out, block{id: m[1], start: i, end: end, lines: lines[i : end+1]})
		i = end
	}
	return out
}

// bundleID resolves PRODUCT_BUNDLE_IDENTIFIER in a build configuration, expanding $(VAR) references to
// other settings in the same configuration. Unresolvable references are left as they are.
func bundleID(cfg block) string {
	settings := map[string]string{}
	for _, l := range cfg.lines {
		if m := settingLine.FindStringSubmatch(l); m != nil {
			settings[m[1]] = unquote(m[2])
		}
	}
	v := settings["PRODUCT_BUNDLE_IDENTIFIER"]
	for range 4 {
		next := varRef.ReplaceAllStringFunc(v, func(ref string) string {
			name := varRef.FindStringSubmatch(ref)[1]
			if s, ok := settings[name]; ok && name != "PRODUCT_BUNDLE_IDENTIFIER" {
				return s
			}
			return ref
		})
		if next == v {
			break
		}
		v = next
	}
	return v
}

// Target is a native target that needs its own provisioning profile.
type Target struct {
	Name        string
	ProductType string
	// Bundle ids per configuration name (Debug, Release, ...).
	BundleIDs map[string]string
	configIDs []string
}

// SignedTargets lists the app and extension targets in the project, with their bundle ids.
func SignedTargets(src string) []Target {
	blocks := parseBlocks(strings.Split(src, "\n"))
	byID := map[string]block{}
	for _, b := range blocks {
		byID[b.id] = b
	}
	var targets []Target
	for _, b := range blocks {
		if b.value("isa") != "PBXNativeTarget" || !signedProducts[b.value("productType")] {
			continue
		}
		t := Target{Name: b.value("name"), ProductType: b.value("productType"), BundleIDs: map[string]string{}}
		list, ok := byID[firstRef(b.value("buildConfigurationList"))]
		if !ok {
			continue
		}
		for _, id := range listRefs(list, "buildConfigurations") {
			cfg, ok := byID[id]
			if !ok {
				continue
			}
			t.configIDs = append(t.configIDs, id)
			t.BundleIDs[cfg.value("name")] = bundleID(cfg)
		}
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets
}

func firstRef(v string) string {
	if m := idRef.FindString(v); m != "" {
		return m
	}
	return ""
}

// listRefs reads `key = ( ID /* x */, ID /* y */, );` from a block.
func listRefs(b block, key string) []string {
	var ids []string
	in := false
	for _, l := range b.lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, key+" = (") {
			in = true
			continue
		}
		if in {
			if strings.HasPrefix(t, ");") {
				break
			}
			if id := firstRef(t); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// MissingProfiles returns the bundle ids of signed targets' Release configurations that have no profile.
func MissingProfiles(targets []Target, profiles map[string]string) []string {
	seen := map[string]bool{}
	var missing []string
	for _, t := range targets {
		id, ok := t.BundleIDs["Release"]
		if !ok {
			continue
		}
		if _, have := profiles[id]; !have && !seen[id] {
			seen[id] = true
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return missing
}

// Patched is one build configuration switched to manual signing.
type Patched struct{ Target, Config, BundleID, Profile string }

// PatchPbxproj switches every configuration of every signed target whose bundle id has a profile
// to manual signing with that profile. Setting these on the xcodebuild command line instead would
// also apply them to Pods targets and fail.
func PatchPbxproj(src, teamID string, profiles map[string]string) (string, []Patched) {
	lines := strings.Split(src, "\n")
	blocks := parseBlocks(lines)
	byID := map[string]block{}
	for _, b := range blocks {
		byID[b.id] = b
	}
	type plan struct{ target, profile, bundle string }
	todo := map[string]plan{} // configuration id → plan
	for _, t := range SignedTargets(src) {
		for _, id := range t.configIDs {
			bid := bundleID(byID[id])
			if name, ok := profiles[bid]; ok {
				todo[id] = plan{target: t.Name, profile: name, bundle: bid}
			}
		}
	}

	var out []string
	var patched []Patched
	next := 0
	for _, b := range blocks {
		p, ok := todo[b.id]
		if !ok {
			continue
		}
		out = append(out, lines[next:b.start]...)
		for _, l := range b.lines {
			if signingKeys.MatchString(l) {
				continue
			}
			out = append(out, l)
			if strings.TrimSpace(l) == "buildSettings = {" {
				in := strings.Repeat("\t", strings.Count(l[:len(l)-len(strings.TrimLeft(l, "\t"))], "\t")+1)
				out = append(out,
					in+`CODE_SIGN_IDENTITY = "Apple Distribution";`,
					in+`"CODE_SIGN_IDENTITY[sdk=iphoneos*]" = "Apple Distribution";`,
					in+"CODE_SIGN_STYLE = Manual;",
					in+"DEVELOPMENT_TEAM = "+teamID+";",
					in+`PROVISIONING_PROFILE_SPECIFIER = "`+strings.ReplaceAll(p.profile, `"`, `\"`)+`";`,
				)
			}
		}
		next = b.end + 1
		patched = append(patched, Patched{Target: p.target, Config: b.value("name"), BundleID: p.bundle, Profile: p.profile})
	}
	out = append(out, lines[next:]...)
	return strings.Join(out, "\n"), patched
}
