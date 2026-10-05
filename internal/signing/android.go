package signing

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/liftbay/runner/internal/api"
	"github.com/liftbay/runner/internal/proc"
)

// GenerateUploadKeystore creates a PKCS12 upload keystore with keytool. PKCS12 uses one password
// for the store and the key, so both fields carry the same value.
func GenerateUploadKeystore(ctx context.Context, run proc.Runner, logCmd func(proc.Cmd), dir, commonName string) (*api.AndroidSigning, error) {
	password := randomHex(24)
	path := filepath.Join(dir, "generated-upload.jks")
	_ = os.Remove(path)
	cmd := proc.Cmd{Name: "keytool", Args: []string{
		"-genkeypair", "-v", "-storetype", "PKCS12", "-keystore", path, "-alias", "upload",
		"-keyalg", "RSA", "-keysize", "2048", "-validity", "10000",
		"-storepass", password, "-keypass", password, "-dname", "CN=" + dnEscape(commonName),
	}}
	logCmd(cmd)
	if err := run.Run(ctx, cmd); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	return &api.AndroidSigning{KeystoreBase64: base64.StdEncoding.EncodeToString(data), StorePassword: password, KeyAlias: "upload", KeyPassword: password}, nil
}

// WriteKeystore decodes the keystore into dir (outside the repo) and returns the Gradle
// injected-signing properties as ORG_GRADLE_PROJECT_* variables.
func WriteKeystore(s *api.AndroidSigning, dir string) ([]string, error) {
	data, err := base64.StdEncoding.DecodeString(s.KeystoreBase64)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "upload.jks")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	p := "ORG_GRADLE_PROJECT_android.injected.signing."
	return []string{
		p + "store.file=" + path,
		p + "store.password=" + s.StorePassword,
		p + "key.alias=" + s.KeyAlias,
		p + "key.password=" + s.KeyPassword,
	}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func dnEscape(s string) string {
	if s == "" {
		return "Liftbay upload key"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case ',', '+', '"', '\\', '<', '>', ';', '=':
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}
