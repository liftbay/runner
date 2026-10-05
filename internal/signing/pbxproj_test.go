package signing

import (
	"reflect"
	"strings"
	"testing"
)

// An Expo app with a widget and a notification service extension, plus a framework target that
// must never be signed with a profile.
const project = `// !$*UTF8*$!
{
	objects = {

/* Begin PBXNativeTarget section */
		A00000000000000000000001 /* App */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = C00000000000000000000001 /* Build configuration list for PBXNativeTarget "App" */;
			name = App;
			productType = "com.apple.product-type.application";
		};
		A00000000000000000000002 /* Widget */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = C00000000000000000000002 /* Build configuration list for PBXNativeTarget "Widget" */;
			name = Widget;
			productType = "com.apple.product-type.app-extension";
		};
		A00000000000000000000003 /* NotificationService */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = C00000000000000000000003 /* Build configuration list for PBXNativeTarget "NotificationService" */;
			name = NotificationService;
			productType = "com.apple.product-type.app-extension";
		};
		A00000000000000000000004 /* Shared */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = C00000000000000000000004 /* Build configuration list for PBXNativeTarget "Shared" */;
			name = Shared;
			productType = "com.apple.product-type.framework";
		};
/* End PBXNativeTarget section */

/* Begin XCBuildConfiguration section */
		B00000000000000000000011 /* Debug */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				CODE_SIGN_STYLE = Automatic;
				DEVELOPMENT_TEAM = OLD;
				PRODUCT_BUNDLE_IDENTIFIER = com.x.app;
			};
			name = Debug;
		};
		B00000000000000000000012 /* Release */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				"CODE_SIGN_IDENTITY[sdk=iphoneos*]" = "iPhone Developer";
				PRODUCT_BUNDLE_IDENTIFIER = "com.x.app";
			};
			name = Release;
		};
		B00000000000000000000021 /* Debug */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				PRODUCT_BUNDLE_IDENTIFIER = com.x.app.widget;
			};
			name = Debug;
		};
		B00000000000000000000022 /* Release */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				PRODUCT_BUNDLE_IDENTIFIER = com.x.app.widget;
			};
			name = Release;
		};
		B00000000000000000000031 /* Debug */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				BASE_ID = com.x.app;
				PRODUCT_BUNDLE_IDENTIFIER = "$(BASE_ID).NotificationService";
			};
			name = Debug;
		};
		B00000000000000000000032 /* Release */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				BASE_ID = com.x.app;
				PRODUCT_BUNDLE_IDENTIFIER = "$(BASE_ID).NotificationService";
			};
			name = Release;
		};
		B00000000000000000000042 /* Release */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				PRODUCT_BUNDLE_IDENTIFIER = com.x.app;
			};
			name = Release;
		};
/* End XCBuildConfiguration section */

/* Begin XCConfigurationList section */
		C00000000000000000000001 /* Build configuration list for PBXNativeTarget "App" */ = {
			isa = XCConfigurationList;
			buildConfigurations = (
				B00000000000000000000011 /* Debug */,
				B00000000000000000000012 /* Release */,
			);
			defaultConfigurationName = Release;
		};
		C00000000000000000000002 /* Build configuration list for PBXNativeTarget "Widget" */ = {
			isa = XCConfigurationList;
			buildConfigurations = (
				B00000000000000000000021 /* Debug */,
				B00000000000000000000022 /* Release */,
			);
		};
		C00000000000000000000003 /* Build configuration list for PBXNativeTarget "NotificationService" */ = {
			isa = XCConfigurationList;
			buildConfigurations = (
				B00000000000000000000031 /* Debug */,
				B00000000000000000000032 /* Release */,
			);
		};
		C00000000000000000000004 /* Build configuration list for PBXNativeTarget "Shared" */ = {
			isa = XCConfigurationList;
			buildConfigurations = (
				B00000000000000000000042 /* Release */,
			);
		};
/* End XCConfigurationList section */
	};
}`

var threeProfiles = map[string]string{
	"com.x.app":                     "X App Store",
	"com.x.app.widget":              "X Widget",
	"com.x.app.NotificationService": "X Notifications",
}

func configBlock(t *testing.T, src, id string) string {
	t.Helper()
	i := strings.Index(src, "\t\t"+id)
	if i < 0 {
		t.Fatalf("block %s not found", id)
	}
	j := strings.Index(src[i:], "\n\t\t};")
	return src[i : i+j]
}

func TestSignedTargets(t *testing.T) {
	targets := SignedTargets(project)
	got := map[string]map[string]string{}
	for _, tg := range targets {
		got[tg.Name] = tg.BundleIDs
	}
	want := map[string]map[string]string{
		"App":                 {"Debug": "com.x.app", "Release": "com.x.app"},
		"Widget":              {"Debug": "com.x.app.widget", "Release": "com.x.app.widget"},
		"NotificationService": {"Debug": "com.x.app.NotificationService", "Release": "com.x.app.NotificationService"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %v", got)
	}
}

func TestPatchPbxprojAppAndExtensions(t *testing.T) {
	out, patched := PatchPbxproj(project, "TEAM123", threeProfiles)
	if len(patched) != 6 {
		t.Fatalf("patched %d configurations, want 6: %+v", len(patched), patched)
	}
	for id, profile := range map[string]string{
		"B00000000000000000000011": "X App Store", "B00000000000000000000012": "X App Store",
		"B00000000000000000000021": "X Widget", "B00000000000000000000022": "X Widget",
		"B00000000000000000000031": "X Notifications", "B00000000000000000000032": "X Notifications",
	} {
		b := configBlock(t, out, id)
		if !strings.Contains(b, `PROVISIONING_PROFILE_SPECIFIER = "`+profile+`";`) || !strings.Contains(b, "\t\t\t\tCODE_SIGN_STYLE = Manual;") || !strings.Contains(b, "DEVELOPMENT_TEAM = TEAM123;") {
			t.Fatalf("%s not patched with %q:\n%s", id, profile, b)
		}
		if strings.Contains(b, "OLD") || strings.Contains(b, "Automatic") || strings.Contains(b, "iPhone Developer") {
			t.Fatalf("old signing settings left in %s:\n%s", id, b)
		}
	}
	if strings.Contains(configBlock(t, out, "B00000000000000000000042"), "Manual") {
		t.Fatal("framework target was patched")
	}
	if !strings.HasSuffix(out, "\t};\n}") || !strings.Contains(out, "/* End XCConfigurationList section */") {
		t.Fatal("rest of the file was not kept")
	}
}

func TestMissingProfiles(t *testing.T) {
	targets := SignedTargets(project)
	if m := MissingProfiles(targets, threeProfiles); len(m) != 0 {
		t.Fatalf("missing = %v", m)
	}
	m := MissingProfiles(targets, map[string]string{"com.x.app": "X App Store"})
	if want := []string{"com.x.app.NotificationService", "com.x.app.widget"}; !reflect.DeepEqual(m, want) {
		t.Fatalf("missing = %v, want %v", m, want)
	}
}

func TestExportOptionsAllProfiles(t *testing.T) {
	v, err := ParsePlist([]byte(ExportOptions("app-store", "TEAM123", threeProfiles)))
	if err != nil {
		t.Fatal(err)
	}
	got := v.(map[string]any)["provisioningProfiles"].(map[string]any)
	if len(got) != 3 {
		t.Fatalf("profiles = %v", got)
	}
	for id, name := range threeProfiles {
		if got[id] != name {
			t.Fatalf("%s = %v, want %q", id, got[id], name)
		}
	}
}

func TestCheckProfile(t *testing.T) {
	if err := checkProfile(ProfileInfo{Name: "Any", BundleID: "*", TeamID: "TEAM123"}, "TEAM123", nil); err == nil || !strings.Contains(err.Error(), "wildcard") {
		t.Fatalf("wildcard accepted: %v", err)
	}
	if err := checkProfile(ProfileInfo{Name: "Other", BundleID: "com.x.app", TeamID: "OTHER"}, "TEAM123", nil); err == nil {
		t.Fatal("profile from another team accepted")
	}
	have := map[string]InstalledProfile{"com.x.app": {Name: "First"}}
	if err := checkProfile(ProfileInfo{Name: "Second", BundleID: "com.x.app", TeamID: "TEAM123"}, "TEAM123", have); err == nil {
		t.Fatal("duplicate profile accepted")
	}
	if err := checkProfile(ProfileInfo{Name: "Widget", BundleID: "com.x.app.widget", TeamID: "TEAM123"}, "TEAM123", have); err != nil {
		t.Fatal(err)
	}
}
