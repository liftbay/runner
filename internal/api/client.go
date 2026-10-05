package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// HTTPError is a non-2xx response from the API.
type HTTPError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s → %d %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

// IsStatus reports whether err is an HTTPError with the given status.
func IsStatus(err error, status int) bool {
	var h *HTTPError
	return errors.As(err, &h) && h.Status == status
}

type Client struct {
	BaseURL string
	BuildID string
	Token   string
	HTTP    *http.Client
	// Attempts per call for network errors and 5xx/429 responses.
	Attempts int
	Backoff  time.Duration
}

func New(baseURL, buildID, token string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		BuildID:  buildID,
		Token:    token,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
		Attempts: 5,
		Backoff:  500 * time.Millisecond,
	}
}

func (c *Client) buildPath(suffix string) string {
	return "/v1/runner/builds/" + c.BuildID + suffix
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	attempts := max(c.Attempts, 1)
	var last error
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.Backoff * time.Duration(1<<(i-1))):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			last = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
			last = &HTTPError{method, path, res.StatusCode, string(body)}
			continue
		}
		if res.StatusCode >= 300 {
			return &HTTPError{method, path, res.StatusCode, string(body)}
		}
		if out != nil && len(body) > 0 {
			return json.Unmarshal(body, out)
		}
		return nil
	}
	return last
}

// Exchange trades a GitHub OIDC token for this build's one-time job token.
func (c *Client) Exchange(ctx context.Context, oidcToken string) (string, error) {
	var out struct {
		JobToken string `json:"jobToken"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/runner/github/exchange", map[string]string{"buildId": c.BuildID, "oidcToken": oidcToken}, &out)
	if err == nil && out.JobToken == "" {
		err = errors.New("exchange returned no job token")
	}
	return out.JobToken, err
}

func (c *Client) Job(ctx context.Context) (*Job, error) {
	var job Job
	if err := c.do(ctx, http.MethodGet, c.buildPath("/job"), nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Update reports status, phases and build metadata. Returns whether a cancel was requested.
func (c *Client) Update(ctx context.Context, u Update) (bool, error) {
	var out struct {
		CancelRequested bool `json:"cancelRequested"`
	}
	err := c.do(ctx, http.MethodPatch, c.buildPath(""), u, &out)
	return out.CancelRequested, err
}

func (c *Client) SendLogs(ctx context.Context, lines []LogLine) (bool, error) {
	var out struct {
		CancelRequested bool `json:"cancelRequested"`
	}
	err := c.do(ctx, http.MethodPost, c.buildPath("/logs"), map[string]any{"lines": lines}, &out)
	return out.CancelRequested, err
}

// Heartbeat is an empty status report: it only says the runner is alive (and returns cancel requests).
func (c *Client) Heartbeat(ctx context.Context) (bool, error) {
	return c.Update(ctx, Update{})
}

// PutAndroidSigning stores a generated upload keystore. A 409 means the project already has one.
func (c *Client) PutAndroidSigning(ctx context.Context, s AndroidSigning) error {
	return c.do(ctx, http.MethodPut, c.buildPath("/signing/android"), s, nil)
}

func (c *Client) Finish(ctx context.Context, status string) error {
	return c.do(ctx, http.MethodPost, c.buildPath("/finish"), map[string]string{"status": status}, nil)
}

// UploadArtifact registers a file, uploads it to the signed URL and confirms it.
func (c *Client) UploadArtifact(ctx context.Context, kind, name, path, contentType string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	var reg struct {
		ID          string `json:"id"`
		UploadURL   string `json:"uploadUrl"`
		ContentType string `json:"contentType"`
	}
	in := map[string]any{"kind": kind, "name": name, "sizeBytes": info.Size(), "contentType": contentType}
	if err := c.do(ctx, http.MethodPost, c.buildPath("/artifacts"), in, &reg); err != nil {
		return err
	}
	if err := c.put(ctx, reg.UploadURL, path, info.Size(), reg.ContentType); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, c.buildPath("/artifacts/"+reg.ID+"/complete"), nil, nil)
}

func (c *Client) put(ctx context.Context, url, path string, size int64, contentType string) error {
	var last error
	for i := range max(c.Attempts, 1) {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.Backoff * time.Duration(1<<(i-1))):
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
		if err != nil {
			f.Close()
			return err
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", contentType)
		// Uploads can be large; no client timeout beyond the context.
		res, err := (&http.Client{}).Do(req)
		f.Close()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			last = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		res.Body.Close()
		if res.StatusCode < 300 {
			return nil
		}
		last = &HTTPError{http.MethodPut, "(upload url)", res.StatusCode, string(body)}
		if res.StatusCode < 500 {
			return last
		}
	}
	return last
}
