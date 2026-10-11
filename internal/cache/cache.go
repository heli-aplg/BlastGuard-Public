// Package cache stores schema and table names for navigation. Nothing used by
// the analysis comes from here; it is always re-read live.
package cache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"blastguard/internal/catalog"
	"blastguard/internal/db"
)

const version = 1

type Metadata struct {
	Version       int                   `json:"version"`
	ScannedAt     time.Time             `json:"scanned_at"`
	Server        string                `json:"server"`
	ServerVersion string                `json:"server_version"`
	Schemas       []catalog.SchemaEntry `json:"schemas"`
	Tables        []catalog.TableEntry  `json:"tables"`
}

// TablesOf returns the cached tables of one schema.
func (m *Metadata) TablesOf(schema string) []catalog.TableEntry {
	var out []catalog.TableEntry
	for _, t := range m.Tables {
		if t.Schema == schema {
			out = append(out, t)
		}
	}
	return out
}

// Path is the cache file for one server/database/user inside dir (config cache_dir).
func Path(dir string, ro *db.Account) string {
	sum := sha1.Sum([]byte(ro.Addr + "/" + ro.Database + "/" + ro.User))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".json")
}

// Load returns the cached metadata, or nil when absent or unreadable.
func Load(dir string, ro *db.Account) *Metadata {
	raw, err := os.ReadFile(Path(dir, ro))
	if err != nil {
		return nil
	}
	var m Metadata
	if json.Unmarshal(raw, &m) != nil || m.Version != version {
		return nil
	}
	return &m
}

// Scan reads schema and table names live (read-only) and writes the cache.
func Scan(ctx context.Context, dir string, ro, bc *db.Account) (*Metadata, error) {
	s, err := db.Begin(ctx, ro, bc)
	if err != nil {
		return nil, err
	}
	defer s.Close(context.Background())
	schemas, err := catalog.ListSchemas(ctx, s)
	if err != nil {
		return nil, err
	}
	tables, err := catalog.ListTables(ctx, s)
	if err != nil {
		return nil, err
	}
	m := &Metadata{
		Version: version, ScannedAt: time.Now(), Server: ro.Addr + "/" + ro.Database,
		ServerVersion: ro.ServerVersion, Schemas: schemas, Tables: tables,
	}
	return m, save(dir, ro, m)
}

func save(dir string, ro *db.Account, m *Metadata) error {
	p := Path(dir, ro)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
