package recipe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liftbay/runner/internal/api"
	"github.com/liftbay/runner/internal/logship"
	"github.com/liftbay/runner/internal/proc"
)

func TestDetectPackageManager(t *testing.T) {
	cases := []struct {
		files []string
		want  string
	}{
		{[]string{"bun.lockb", "package-lock.json"}, "bun install --frozen-lockfile"},
		{[]string{"pnpm-lock.yaml"}, "pnpm install --frozen-lockfile"},
		{[]string{"yarn.lock", ".yarnrc.yml"}, "yarn install --immutable"},
		{[]string{"yarn.lock"}, "yarn install --frozen-lockfile"},
		{[]string{"package-lock.json"}, "npm ci --no-audit --no-fund"},
		{nil, "npm install --no-audit --no-fund"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		for _, f := range c.files {
			_ = os.WriteFile(filepath.Join(dir, f), nil, 0o644)
		}
		if got := strings.Join(DetectPackageManager(dir).Install, " "); got != c.want {
			t.Errorf("%v: got %q, want %q", c.files, got, c.want)
		}
	}
}

func TestChooseScheme(t *testing.T) {
	if s := ChooseScheme("InstaSupply", []string{"Pods-InstaSupply", "InstaSupply", "React"}); s != "InstaSupply" {
		t.Fatal(s)
	}
	if s := ChooseScheme("App", []string{"EXConstants", "React-Core", "MyApp"}); s != "MyApp" {
		t.Fatal(s)
	}
}

func TestGradleHelpers(t *testing.T) {
	if !gradleHeapTooSmall("org.gradle.jvmargs=-Xmx2048m -XX:MaxMetaspaceSize=512m") || gradleHeapTooSmall("org.gradle.jvmargs=-Xmx4g") || !gradleHeapTooSmall("") {
		t.Fatal("heap detection")
	}
	name, code := gradleVersion("  defaultConfig {\n    versionCode 42\n    versionName \"1.5.0\"\n")
	if name != "1.5.0" || code != "42" {
		t.Fatal(name, code)
	}
}

// fakeProc pretends to run build tools, creating the files they would produce.
type fakeProc struct {
	mu   sync.Mutex
	cmds []proc.Cmd
}

func (f *fakeProc) Run(_ context.Context, c proc.Cmd) error {
	f.mu.Lock()
	f.cmds = append(f.cmds, c)
	f.mu.Unlock()
	switch c.Name {
	case "keytool":
		i := slices.Index(c.Args, "-keystore")
		return os.WriteFile(c.Args[i+1], []byte("KEYSTORE"), 0o600)
	case "./gradlew":
		if !slices.Contains(c.Env, "ORG_GRADLE_PROJECT_android.injected.signing.key.alias=upload") {
			c.Stderr("missing signing properties")
			return os.ErrInvalid
		}
		c.Stdout("> Task :app:bundleRelease")
		c.Stdout("using API_KEY super-secret-value")
		out := filepath.Join(c.Dir, "app", "build", "outputs", "bundle", "release")
		_ = os.MkdirAll(out, 0o755)
		return os.WriteFile(filepath.Join(out, "app-release.aab"), []byte("AAB"), 0o644)
	}
	if c.Stdout != nil {
		c.Stdout("ok " + c.Name)
	}
	return nil
}

type fakeAPI struct {
	mu        sync.Mutex
	updates   []api.Update
	logs      []api.LogLine
	signing   *api.AndroidSigning
	uploaded  map[string]string
	completed []string
}

func (f *fakeAPI) handler(t *testing.T, srvURL *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path != "/upload/art_1" && r.Header.Get("Authorization") != "Bearer lbj_test" {
			http.Error(w, "unauthenticated", 401)
			return
		}
		base := "/v1/runner/builds/bld_1"
		switch {
		case r.Method == "PATCH" && r.URL.Path == base:
			var u api.Update
			_ = json.NewDecoder(r.Body).Decode(&u)
			f.updates = append(f.updates, u)
			_, _ = w.Write([]byte(`{"cancelRequested":false}`))
		case r.Method == "POST" && r.URL.Path == base+"/logs":
			var in struct{ Lines []api.LogLine }
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.logs = append(f.logs, in.Lines...)
			_, _ = w.Write([]byte(`{"nextSeq":1,"cancelRequested":false}`))
		case r.Method == "PUT" && r.URL.Path == base+"/signing/android":
			f.signing = &api.AndroidSigning{}
			_ = json.NewDecoder(r.Body).Decode(f.signing)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == "POST" && r.URL.Path == base+"/artifacts":
			var in struct{ Kind, Name, ContentType string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.uploaded[in.Name] = in.Kind
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"art_1","uploadUrl":"` + *srvURL + `/upload/art_1","contentType":"` + in.ContentType + `"}`))
		case r.Method == "PUT" && r.URL.Path == "/upload/art_1":
			body, _ := io.ReadAll(r.Body)
			if string(body) != "AAB" {
				t.Errorf("uploaded %q", body)
			}
		case r.Method == "POST" && r.URL.Path == base+"/artifacts/art_1/complete":
			f.completed = append(f.completed, "art_1")
			_, _ = w.Write([]byte(`{"id":"art_1","sizeBytes":3}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func TestAndroidExpoBuildAgainstFakeAPI(t *testing.T) {
	fa := &fakeAPI{uploaded: map[string]string{}}
	var url string
	srv := httptest.NewServer(fa.handler(t, &url))
	defer srv.Close()
	url = srv.URL

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"expo":{"version":"1.5.0","android":{"versionCode":42}}}`), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "android"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "android", "gradlew"), []byte("#!/bin/sh\n"), 0o755)

	job := &api.Job{
		Build:   api.JobBuild{ID: "bld_1", Number: 7, Platform: "android", Profile: "production", Environment: "production", Distribution: "store"},
		Project: api.JobProject{Slug: "instasupply", Name: "InstaSupply", Framework: "expo"},
		Env:     []api.EnvVar{{Key: "API_KEY", Value: "super-secret-value", Secret: true}, {Key: "EXPO_PUBLIC_URL", Value: "https://x"}},
	}
	client := api.New(srv.URL, "bld_1", "lbj_test")
	client.Backoff = time.Millisecond
	redactor := &logship.Redactor{}
	redactor.Add("super-secret-value")
	shipper := logship.New(client, redactor, nil)
	shipper.FlushEvery = 10 * time.Millisecond
	shipper.Start()

	steps, err := Steps(job)
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakeProc{}
	b := &Build{Job: job, Dir: dir, Temp: t.TempDir(), API: client, Proc: fp, Log: shipper, Env: []string{"PATH=/usr/bin"}, AddSecret: func(v string) { redactor.Add(v) }}
	status := Execute(context.Background(), b, steps)
	if err := shipper.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status != api.StatusSucceeded {
		for _, l := range fa.logs {
			t.Log(l.Phase, l.Stream, l.Text)
		}
		t.Fatalf("status %s", status)
	}

	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	if want := []string{"Set up environment", "Install dependencies", "Prebuild", "Prepare signing", "Gradle bundleRelease", "Upload artifacts"}; !slices.Equal(names, want) {
		t.Fatalf("steps %v", names)
	}
	last := fa.updates[len(fa.updates)-1]
	for _, p := range last.Phases {
		if p.Status != api.StatusSucceeded || p.DurationSec == nil {
			t.Fatalf("phase %+v", p)
		}
	}
	if fa.updates[1].AppVersion != "1.5.0" || fa.updates[1].BuildNumber != "42" {
		t.Fatalf("metadata update %+v", fa.updates[1])
	}
	if fa.signing == nil || fa.signing.KeyAlias != "upload" || fa.signing.KeystoreBase64 != "S0VZU1RPUkU=" {
		t.Fatalf("signing %+v", fa.signing)
	}
	if fa.uploaded["instasupply-7.aab"] != "aab" || len(fa.completed) != 1 {
		t.Fatalf("artifacts %v %v", fa.uploaded, fa.completed)
	}
	for _, l := range fa.logs {
		if strings.Contains(l.Text, "super-secret-value") || strings.Contains(l.Text, fa.signing.StorePassword) {
			t.Fatalf("secret leaked: %q", l.Text)
		}
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !strings.Contains(string(env), `EXPO_PUBLIC_URL="https://x"`) {
		t.Fatalf(".env: %s", env)
	}
	if fp.cmds[0].Name != "npm" || fp.cmds[1].Name != "npx" {
		t.Fatalf("commands %v %v", fp.cmds[0], fp.cmds[1])
	}
}

func TestCancelStopsTheBuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var updates []api.Update
	b := &Build{Job: &api.Job{}, Log: nopLog{}, API: updateRecorder(func(u api.Update) { updates = append(updates, u) })}
	steps := []Step{
		{Name: "one", Run: func(context.Context, *Build) error { cancel(); return context.Canceled }},
		{Name: "two", Run: func(context.Context, *Build) error { t.Fatal("ran after cancel"); return nil }},
	}
	if s := Execute(ctx, b, steps); s != api.StatusCancelled {
		t.Fatal(s)
	}
	if p := updates[len(updates)-1].Phases; p[0].Status != api.StatusCancelled || p[1].Status != api.StatusQueued {
		t.Fatalf("phases %+v", p)
	}
}

func TestBrownfieldUnsupported(t *testing.T) {
	_, err := Steps(&api.Job{Project: api.JobProject{Framework: "brownfield"}, Build: api.JobBuild{Platform: "ios"}})
	if err == nil || !strings.Contains(err.Error(), "not supported by this runner yet") {
		t.Fatal(err)
	}
}

type nopLog struct{}

func (nopLog) Log(string, string, string) {}

type updateRecorder func(api.Update)

func (f updateRecorder) Update(_ context.Context, u api.Update) (bool, error) {
	f(api.Update{Status: u.Status, Phases: slices.Clone(u.Phases)})
	return false, nil
}
func (updateRecorder) Job(context.Context) (*api.Job, error)                       { return nil, nil }
func (updateRecorder) PutAndroidSigning(context.Context, api.AndroidSigning) error { return nil }
func (updateRecorder) UploadArtifact(context.Context, string, string, string, string) error {
	return nil
}
