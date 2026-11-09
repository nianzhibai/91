// Package githubrepo validates public GitHub repository URLs.
package githubrepo

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Name extracts owner/name from a complete HTTPS repository URL.
func Name(address string) (string, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return "", errors.New("repository must be a complete GitHub URL such as https://github.com/owner/name")
	}
	name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), ".git")
	if !namePattern.MatchString(name) {
		return "", errors.New("repository URL must identify a GitHub repository")
	}
	return name, nil
}
