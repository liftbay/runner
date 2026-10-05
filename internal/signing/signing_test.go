package signing

import (
	"testing"
)

func TestExportOptionsRoundTrip(t *testing.T) {
	xml := ExportOptions("app-store", "TEAM123", map[string]string{"com.x.app": `Liftbay "AppStore" & co`})
	v, err := ParsePlist([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	d := v.(map[string]any)
	if d["method"] != "app-store" || d["teamID"] != "TEAM123" || d["signingStyle"] != "manual" || d["uploadSymbols"] != true {
		t.Fatalf("unexpected options: %#v", d)
	}
	if p := d["provisioningProfiles"].(map[string]any)["com.x.app"]; p != `Liftbay "AppStore" & co` {
		t.Fatalf("profile = %v", p)
	}
}

func TestReadProfile(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>AppIDName</key><string>X</string>
  <key>Entitlements</key><dict>
    <key>application-identifier</key><string>TEAM123.com.x.app</string>
    <key>get-task-allow</key><false/>
  </dict>
  <key>Name</key><string>X App Store</string>
  <key>TeamIdentifier</key><array><string>TEAM123</string></array>
  <key>UUID</key><string>1111-2222</string>
  <key>Version</key><integer>1</integer>
</dict></plist>`
	info, err := ReadProfile([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	want := ProfileInfo{Name: "X App Store", UUID: "1111-2222", TeamID: "TEAM123", BundleID: "com.x.app"}
	if info != want {
		t.Fatalf("got %+v", info)
	}
}
