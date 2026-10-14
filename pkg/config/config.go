// Package config stores per-user defaults for the basal CLI in a small
// INI file (mirroring gcloud's config): which .bsl container to open and
// which branch to query when no flag is given.
//
// The file lives at $BASAL_CONFIG when that variable is set, otherwise
// at os.UserConfigDir()/basal/config. Its format is:
//
//	[cli]
//	file = demo.bsl
//	branch = scratch
//
// Flags always win over the stored defaults, so one-off overrides stay
// one command long: `basal query -branch main 'SHOW RECORDS'`.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"basaldb/pkg/engine"
)

// Keys that may appear in the [cli] section.
const (
	KeyFile   = "file"
	KeyBranch = "branch"
)

// FallbackFile is the container used when neither the flag nor the
// config names one.
const FallbackFile = "basal.bsl"

// Config holds the stored defaults. An empty field means unset.
type Config struct {
	File   string
	Branch string
}

// Path returns the location of the config file.
func Path() (string, error) {
	if p := os.Getenv("BASAL_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot locate the user config directory: %w", err)
	}
	return filepath.Join(dir, "basal", "config"), nil
}

// Load reads the config file. A missing file yields an empty Config.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	section := ""
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		if section != "" && section != "cli" {
			continue // other sections are reserved, ignore them
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("config: %s:%d: expected \"key = value\"", path, i+1)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case KeyFile:
			c.File = value
		case KeyBranch:
			c.Branch = value
		default:
			return Config{}, fmt.Errorf("config: %s:%d: unknown key %q (want %q or %q)", path, i+1, key, KeyFile, KeyBranch)
		}
	}
	return c, nil
}

// Save writes the config, creating the directory if needed.
func (c Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("[cli]\n")
	if c.File != "" {
		fmt.Fprintf(&b, "%s = %s\n", KeyFile, c.File)
	}
	if c.Branch != "" {
		fmt.Fprintf(&b, "%s = %s\n", KeyBranch, c.Branch)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

func checkKey(key string) (string, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key != KeyFile && key != KeyBranch {
		return "", fmt.Errorf("config: unknown key %q (want %q or %q)", key, KeyFile, KeyBranch)
	}
	return key, nil
}

// Set stores one key in the config file.
func Set(key, value string) error {
	key, err := checkKey(key)
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("config: empty value for %q", key)
	}
	c, err := Load()
	if err != nil {
		return err
	}
	if key == KeyFile {
		c.File = strings.TrimSpace(value)
	} else {
		c.Branch = strings.TrimSpace(value)
	}
	return c.Save()
}

// Get returns one stored value; an unset key is an error.
func Get(key string) (string, error) {
	key, err := checkKey(key)
	if err != nil {
		return "", err
	}
	c, err := Load()
	if err != nil {
		return "", err
	}
	value := c.File
	if key == KeyBranch {
		value = c.Branch
	}
	if value == "" {
		return "", fmt.Errorf("config: %q is not set", key)
	}
	return value, nil
}

// Unset removes one stored key.
func Unset(key string) error {
	key, err := checkKey(key)
	if err != nil {
		return err
	}
	c, err := Load()
	if err != nil {
		return err
	}
	if key == KeyFile {
		c.File = ""
	} else {
		c.Branch = ""
	}
	return c.Save()
}

// ResolveFile returns the container to open: the flag if set, else the
// configured default, else FallbackFile in the working directory.
func ResolveFile(flag string) (string, error) {
	if strings.TrimSpace(flag) != "" {
		return flag, nil
	}
	c, err := Load()
	if err != nil {
		return "", err
	}
	if c.File != "" {
		return c.File, nil
	}
	return FallbackFile, nil
}

// ResolveBranch returns the branch to use: the flag if set, else the
// configured default, else the engine's default branch.
func ResolveBranch(flag string) (string, error) {
	if strings.TrimSpace(flag) != "" {
		return flag, nil
	}
	c, err := Load()
	if err != nil {
		return "", err
	}
	if c.Branch != "" {
		return c.Branch, nil
	}
	return engine.DefaultBranch, nil
}
