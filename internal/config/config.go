// Package config loads and validates the blastguard YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	RoleReadonly  = "readonly"
	RoleBecareful = "becareful"

	// DefaultSchema is opened on start when default_schema is absent from the file.
	DefaultSchema = "feight_forwarder"
)

type Config struct {
	Language      string      `yaml:"language"`
	DefaultSchema string      `yaml:"default_schema"`
	MaxRows       int         `yaml:"max_rows"`
	CacheDir      string      `yaml:"cache_dir"`
	Timeouts      Timeouts    `yaml:"timeouts"`
	Tunnel        *Tunnel     `yaml:"tunnel"`
	Connections   Connections `yaml:"connections"`
	AI            AI          `yaml:"ai"`

	Path string `yaml:"-"`
}

type Timeouts struct {
	Connect   time.Duration `yaml:"connect"`
	Statement time.Duration `yaml:"statement"`
	Lock      time.Duration `yaml:"lock"`
}

type Connections struct {
	Readonly  *Connection `yaml:"readonly"`
	Becareful *Connection `yaml:"becareful"`
}

type Connection struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"sslmode"`
}

// Tunnel is shared by both connections: one local port, opened once.
type Tunnel struct {
	Command   string        `yaml:"command"`
	Wait      time.Duration `yaml:"wait"`
	AutoStart *bool         `yaml:"auto_start"` // nil = true; start the tunnel without asking when a port is closed
}

// AI is reserved for the future AI trigger analyzer (see docs/SPEC.md §9).
type AI struct {
	Enabled bool `yaml:"enabled"`
}

// Addr returns host:port of the local endpoint (usually the tunnel end).
func (c *Connection) Addr() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

// DefaultPaths are tried in order when no -config flag is given: the working
// directory, then the source root (see SourceRoot). No home-relative path, so
// it behaves the same on macOS, Linux and Windows.
func DefaultPaths() []string {
	paths := []string{"blastguard.yaml"}
	if root, err := SourceRoot(); err == nil {
		if p := filepath.Join(root, "blastguard.yaml"); p != paths[0] {
			paths = append(paths, p)
		}
	}
	return paths
}

// Load reads the config from path, or from the first existing default path.
func Load(path string) (*Config, error) {
	if path == "" {
		for _, p := range DefaultPaths() {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
		if path == "" {
			return nil, fmt.Errorf("no config file found (tried %v); create blastguard.yaml (see README.md)", DefaultPaths())
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Config{DefaultSchema: DefaultSchema} // an explicit default_schema: "" still wins
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // stale keys (e.g. password_env) fail loudly instead of being ignored
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.Path = path
	cfg.applyDefaults()
	if cfg.CacheDir, err = resolveCacheDir(cfg.CacheDir, path); err != nil {
		return nil, fmt.Errorf("invalid config %s: cache_dir: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Language == "" {
		c.Language = "en"
	}
	if c.MaxRows <= 0 {
		c.MaxRows = 10000
	}
	if c.Timeouts.Connect <= 0 {
		c.Timeouts.Connect = 5 * time.Second
	}
	if c.Timeouts.Statement <= 0 {
		c.Timeouts.Statement = 15 * time.Second
	}
	if c.Timeouts.Lock <= 0 {
		c.Timeouts.Lock = 3 * time.Second
	}
	for _, conn := range []*Connection{c.Connections.Readonly, c.Connections.Becareful} {
		if conn == nil {
			continue
		}
		if conn.Host == "" {
			conn.Host = "127.0.0.1"
		}
		if conn.SSLMode == "" {
			conn.SSLMode = "prefer"
		}
	}
	for _, conn := range []*Connection{c.Connections.Readonly, c.Connections.Becareful} {
		if conn != nil && conn.Port == 0 {
			conn.Port = 5432
		}
	}
	if c.Tunnel != nil && c.Tunnel.Wait <= 0 {
		c.Tunnel.Wait = 30 * time.Second
	}
	if c.Tunnel != nil && c.Tunnel.AutoStart == nil {
		on := true
		c.Tunnel.AutoStart = &on
	}
}

func (c *Config) validate() error {
	if c.Language != "en" {
		return fmt.Errorf("language %q is not supported yet (supported: en)", c.Language)
	}
	if c.Connections.Readonly == nil {
		return errors.New("connections.readonly is required")
	}
	for name, conn := range map[string]*Connection{RoleReadonly: c.Connections.Readonly, RoleBecareful: c.Connections.Becareful} {
		if conn == nil {
			continue
		}
		if conn.Database == "" || conn.User == "" || conn.Password == "" {
			return fmt.Errorf("connections.%s: database, user and password are required", name)
		}
	}
	return nil
}

// resolveCacheDir makes cache_dir absolute. Empty means <source>/cache; a
// relative path is taken from the config file's directory.
func resolveCacheDir(dir, cfgPath string) (string, error) {
	if dir == "" {
		root, err := SourceRoot()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, "cache"), nil
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, dir[1:])
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(cfgPath), dir)
	}
	return filepath.Abs(dir)
}

// SourceRoot is the BlastGuard checkout: the parent of bin/ when the binary was
// built with `make build`, else the binary's own directory. Under `go run` the
// binary lives in a temp dir, so the working directory is used instead.
func SourceRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	if tmp, err := filepath.EvalSymlinks(os.TempDir()); err == nil && strings.HasPrefix(dir, tmp+string(filepath.Separator)) {
		return os.Getwd()
	}
	if filepath.Base(dir) == "bin" {
		dir = filepath.Dir(dir)
	}
	return dir, nil
}
