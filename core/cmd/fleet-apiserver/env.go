package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// tokens is a flag that may be given more than once.
//
// Both spellings exist for the same reason the single value used to be an
// environment variable: a systemd unit has to be able to hand the credential
// over without quoting problems, while a command line wants to repeat it. An
// environment value seeds the list before flag parsing, so naming the flag adds
// to what the environment already supplied instead of erasing it.
//
// Commas separate as well, so FLEET_ADMIN_TOKEN can carry more than one without
// the installer having to repeat the flag name. That rules out a token
// containing a comma, which costs nothing: the installer generates base64url,
// which has no comma.
type tokens []string

func (t *tokens) String() string { return strings.Join(*t, ",") }

// add appends the tokens in one value.
//
// This is where the comma is handled, and it was previously documented but not
// implemented: the list arrived as the single string "a,b", so neither token
// matched and every request was refused. No test caught it because the tests
// built Config.AdminTokens directly and never went through the flag or the
// environment.
func (t *tokens) add(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("the admin token is empty; an empty value would look configured while refusing every request")
	}
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*t = append(*t, part)
		}
	}
	return nil
}

func (t *tokens) Set(v string) error { return t.add(v) }

func (t *tokens) seed(key string) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		// A seeded value that fails to parse would silently leave the server
		// with no token, so the failure surfaces as no tokens at all -- which
		// NewServer then refuses on a wildcard bind rather than at runtime.
		_ = t.add(v)
	}
}
