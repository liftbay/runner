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

func TestCertSHA256(t *testing.T) {
	keytool := "Certificate fingerprints:\n\t SHA1: AA:BB\n\t SHA256: BF:FF:12:E4:0C:44:EE:AF:2B:76:C7:88:47:D0:07:24:D0:0D:EF:CC:CC:E6:7A:FE:78:16:EC:98:F4:38:54:89\n"
	apksigner := "Signer #1 certificate DN: CN=x\nSigner #1 certificate SHA-256 digest: bfff12e40c44eeaf2b76c78847d00724d00defcccce67afe7816ec98f4385489\n"
	want := "bfff12e40c44eeaf2b76c78847d00724d00defcccce67afe7816ec98f4385489"
	if got := CertSHA256(keytool); got != want {
		t.Fatalf("keytool: %q", got)
	}
	if got := CertSHA256(apksigner); got != want {
		t.Fatalf("apksigner: %q", got)
	}
	if CertSHA256("nothing") != "" {
		t.Fatal("expected empty")
	}
}
