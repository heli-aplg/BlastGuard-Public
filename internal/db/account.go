// Package db owns every database connection. All access goes through
// read-only sessions; nothing in this package can issue a write.
package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"blastguard/internal/config"
)

type Role string

const (
	Readonly  Role = config.RoleReadonly
	Becareful Role = config.RoleBecareful
)

// Account is one live connection (readonly or becareful).
type Account struct {
	Role          Role
	User          string
	Database      string
	Addr          string
	ServerVersion string
	VersionNum    int

	connCfg *pgx.ConnConfig
	conn    *pgx.Conn
}

// sessionParams are applied at connection start. Read-only is enforced here
// (layer L1) and verified after connecting (layer L3). The formatting params
// make server-generated literals unambiguous for the restore script.
func sessionParams(role Role, t config.Timeouts) map[string]string {
	return map[string]string{
		"default_transaction_read_only":       "on",
		"statement_timeout":                   strconv.FormatInt(t.Statement.Milliseconds(), 10),
		"lock_timeout":                        strconv.FormatInt(t.Lock.Milliseconds(), 10),
		"idle_in_transaction_session_timeout": "600000",
		"application_name":                    "blastguard-" + string(role),
		"search_path":                         "pg_catalog",
		"DateStyle":                           "ISO, MDY",
		"IntervalStyle":                       "postgres",
		"TimeZone":                            "UTC",
		"extra_float_digits":                  "3",
		"bytea_output":                        "hex",
		"standard_conforming_strings":         "on",
	}
}

// Connect opens and verifies a read-only connection.
func Connect(ctx context.Context, role Role, c *config.Connection, t config.Timeouts) (*Account, error) {
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=%s connect_timeout=%d",
		dsnQuote(c.Host), c.Port, dsnQuote(c.Database), dsnQuote(c.User), dsnQuote(c.SSLMode),
		int(t.Connect/time.Second)+1)
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cc.Password = c.Password // yaml only: never PGPASSWORD or ~/.pgpass
	for k, v := range sessionParams(role, t) {
		cc.RuntimeParams[k] = v
	}
	a := &Account{Role: role, Database: c.Database, Addr: c.Addr(), connCfg: cc}
	if err := a.open(ctx, t.Connect); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Account) open(ctx context.Context, timeout time.Duration) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, a.connCfg)
	if err != nil {
		return err
	}
	var ro string
	err = conn.QueryRow(cctx, `SELECT current_user::text, current_setting('server_version'),
		current_setting('server_version_num')::int, current_setting('default_transaction_read_only')`).
		Scan(&a.User, &a.ServerVersion, &a.VersionNum, &ro)
	if err != nil {
		conn.Close(ctx)
		return err
	}
	if ro != "on" {
		conn.Close(ctx)
		return fmt.Errorf("refusing connection: default_transaction_read_only=%q (expected on)", ro)
	}
	a.conn = conn
	return nil
}

// Ensure reconnects if the server dropped an idle connection.
func (a *Account) Ensure(ctx context.Context, timeout time.Duration) error {
	if a.conn != nil && !a.conn.IsClosed() {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		err := a.conn.Ping(pctx)
		cancel()
		if err == nil {
			return nil
		}
		a.conn.Close(ctx)
	}
	a.conn = nil
	return a.open(ctx, timeout)
}

func (a *Account) Close() {
	if a != nil && a.conn != nil {
		a.conn.Close(context.Background())
	}
}

// Connected reports whether the account currently holds an open connection.
func (a *Account) Connected() bool { return a != nil && a.conn != nil && !a.conn.IsClosed() }

// Label is a short human identifier, e.g. "ro_user@127.0.0.1:5432".
func (a *Account) Label() string { return a.User + "@" + a.Addr }

func dsnQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, ` '\`) {
		return s
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
