package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// normalizeBaseURL parses raw, requires an http or https scheme, requires a
// non-empty host, and strips any trailing slash from the path. Returned
// string is suitable for direct concatenation with "/v2/api/..." paths
// without producing "//" sequences.
//
// Separated from config.go so the URL helpers can grow in Phase 2 (e.g.,
// for budget-check endpoint composition) without bloating config.go.
func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("missing host")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}
