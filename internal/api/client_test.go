package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOIDCExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oidc":
			if r.URL.Query().Get("audience") != OIDCAudience || r.Header.Get("Authorization") != "bearer req-token" {
				http.Error(w, "bad oidc request", 400)
				return
			}
			_, _ = w.Write([]byte(`{"value":"jwt-abc"}`))
		case "/v1/runner/github/exchange":
			var in struct{ BuildID, OIDCToken string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			if r.Header.Get("Authorization") != "" || in.BuildID != "bld_1" || in.OIDCToken != "jwt-abc" {
				http.Error(w, "bad exchange", 401)
				return
			}
			_, _ = w.Write([]byte(`{"jobToken":"lbj_x","expiresAt":"2026-10-05T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"/oidc?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "req-token")

	ctx := context.Background()
	jwt, err := GitHubOIDCToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := New(srv.URL, "bld_1", "")
	tok, err := c.Exchange(ctx, jwt)
	if err != nil || tok != "lbj_x" {
		t.Fatalf("Exchange = %q, %v", tok, err)
	}
}

func TestRetriesServerErrorsButNotClientErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case r.URL.Path == "/v1/runner/builds/b/signing/android":
			http.Error(w, `{"code":"conflict"}`, 409)
		case calls < 3:
			http.Error(w, "busy", 503)
		default:
			_, _ = w.Write([]byte(`{"cancelRequested":true}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "b", "lbj_t")
	c.Backoff = time.Millisecond
	cancel, err := c.Update(context.Background(), Update{Status: StatusRunning})
	if err != nil || !cancel || calls != 3 {
		t.Fatalf("Update = %v, %v after %d calls", cancel, err, calls)
	}
	calls = 0
	err = c.PutAndroidSigning(context.Background(), AndroidSigning{})
	if !IsStatus(err, 409) || calls != 1 {
		t.Fatalf("PutAndroidSigning = %v after %d calls", err, calls)
	}
}
