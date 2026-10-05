// Package logship batches log lines to the API, redacting secret values first.
package logship

import (
	"sort"
	"strings"
	"sync"
)

const minSecretLen = 3

// Redactor replaces secret values with ***. Safe for concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	secrets  []string
	replacer *strings.Replacer
}

func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen {
			continue
		}
		r.secrets = append(r.secrets, v)
		// Multi-line secrets (keys, certificates) are also hidden line by line.
		for _, part := range strings.Split(v, "\n") {
			if part = strings.TrimSpace(part); len(part) >= minSecretLen && part != v {
				r.secrets = append(r.secrets, part)
			}
		}
	}
	// Longest first, so a secret containing another is replaced whole.
	sort.SliceStable(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
	pairs := make([]string, 0, 2*len(r.secrets))
	for _, s := range r.secrets {
		pairs = append(pairs, s, "***")
	}
	r.replacer = strings.NewReplacer(pairs...)
}

func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.replacer == nil {
		return s
	}
	return r.replacer.Replace(s)
}
