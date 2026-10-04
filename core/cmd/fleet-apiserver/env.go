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
// Commas separate as well, so FLEET_ADMIN_TOKEN can carry more than one. That
// rules out a token containing a comma, which costs nothing: the installer
// generates base64url, which has no comma.
type tokens []string

func (t *tokens) String() string { return strings.Join(*t, ",") }

func (t *tokens) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("the admin token is empty; an empty value would look configured while refusing every request")
	}
	*t = append(*t, strings.TrimSpace(v))
	return nil
}

func (t *tokens) seed(key string) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		*t = append(*t, strings.TrimSpace(v))
	}
}
