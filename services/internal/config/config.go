// Package config reads settings from the environment and a gitignored .env file.
package config

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Env is a set of settings. Process environment variables win over the file.
type Env struct {
	path string
	file map[string]string
}

// Load reads path if it exists. A missing file is not an error.
func Load(path string) (*Env, error) {
	e := &Env{path: path, file: map[string]string{}}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return e, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		e.file[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return e, sc.Err()
}

// Get returns a setting or "".
func (e *Env) Get(key string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return e.file[key]
}

// Or returns a setting or the fallback.
func (e *Env) Or(key, fallback string) string {
	if v := e.Get(key); v != "" {
		return v
	}
	return fallback
}

// Must returns a setting or an error naming it.
func (e *Env) Must(key string) (string, error) {
	if v := e.Get(key); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is not set (run `mergoshi bootstrap` first)", key)
}

// Set stores a setting and rewrites the file, keeping it private to the user.
func (e *Env) Set(key, value string) error {
	e.file[key] = value
	keys := make([]string, 0, len(e.file))
	for k := range e.file {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Written by mergoshi. Holds private keys: never commit or share this file.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, e.file[k])
	}
	return os.WriteFile(e.path, []byte(b.String()), 0o600)
}
