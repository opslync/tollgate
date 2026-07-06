// Package store persists per-request usage records to SQLite and answers
// aggregation queries for GET /usage. The driver is pure Go (modernc.org)
// so Tollgate stays a CGO-free static binary.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"

	"github.com/opslync/tollgate/internal/meter"
)

const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	agent TEXT NOT NULL DEFAULT '',
	team TEXT NOT NULL DEFAULT '',
	namespace TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	stream INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	pod TEXT NOT NULL DEFAULT '',
	workload_kind TEXT NOT NULL DEFAULT '',
	workload TEXT NOT NULL DEFAULT '',
	service_account TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_agent_ts ON requests(agent, ts);
CREATE TABLE IF NOT EXISTS kills (
	agent TEXT PRIMARY KEY,
	ts INTEGER NOT NULL
);
`

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// the schema. WAL + busy_timeout let concurrent request goroutines insert
// without stepping on each other.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?" + url.Values{
		"_pragma": []string{"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema to %s: %w", path, err)
	}
	// CREATE TABLE IF NOT EXISTS silently ignores new columns on a pre-M7 DB,
	// so evolve the schema explicitly. This is the first migration; if a
	// second one is ever needed, a schema_version table is the right move.
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// migrate adds any M7 columns missing from an older requests table, then
// creates the workload index (which depends on those columns existing).
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(requests)`)
	if err != nil {
		return err
	}
	have := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, col := range []string{"pod", "workload_kind", "workload", "service_account"} {
		if have[col] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE requests ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_requests_workload_ts ON requests(workload, ts)`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Record is one proxied request, cost already converted at request time so
// later pricing-table updates never rewrite history.
type Record struct {
	Time       time.Time
	Agent      string
	Team       string
	Namespace  string
	Provider   string
	Model      string
	Method     string
	Path       string
	Status     int
	DurationMS int64
	Stream     bool
	Usage      meter.Usage
	CostUSD    float64
	// K8s-native attribution; empty for static-key-authenticated requests.
	Pod            string
	WorkloadKind   string
	Workload       string
	ServiceAccount string
}

func (s *Store) Insert(ctx context.Context, r Record) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO requests (
			ts, agent, team, namespace, provider, model, method, path,
			status, duration_ms, stream,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens, cost_usd,
			pod, workload_kind, workload, service_account
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Time.UnixMilli(), r.Agent, r.Team, r.Namespace, r.Provider, r.Model,
		r.Method, r.Path, r.Status, r.DurationMS, r.Stream,
		r.Usage.InputTokens, r.Usage.OutputTokens,
		r.Usage.CacheCreationInputTokens, r.Usage.CacheReadInputTokens, r.CostUSD,
		r.Pod, r.WorkloadKind, r.Workload, r.ServiceAccount,
	)
	return err
}

// Spend returns the dollar and token (input+output) totals since the given
// time for one agent or team. dim must be "agent" or "team".
func (s *Store) Spend(ctx context.Context, dim, value string, since time.Time) (usd float64, tokens int64, err error) {
	col, ok := map[string]string{"agent": "agent", "team": "team"}[dim]
	if !ok {
		return 0, 0, fmt.Errorf("invalid spend dimension %q", dim)
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_usd), 0), COALESCE(SUM(input_tokens + output_tokens), 0)
		FROM requests WHERE `+col+` = ? AND ts >= ?`,
		value, since.UnixMilli())
	err = row.Scan(&usd, &tokens)
	return usd, tokens, err
}

// Kill persists the kill switch for an agent so a restart doesn't revive it.
func (s *Store) Kill(ctx context.Context, agent string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kills (agent, ts) VALUES (?, ?) ON CONFLICT(agent) DO NOTHING`,
		agent, at.UnixMilli())
	return err
}

// Revive removes an agent's kill entry.
func (s *Store) Revive(ctx context.Context, agent string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kills WHERE agent = ?`, agent)
	return err
}

// Kills lists currently killed agents.
func (s *Store) Kills(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent FROM kills ORDER BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// groupByColumns is the allowlist for GET /usage grouping; the map value is
// interpolated into SQL, so only vetted column names may appear here.
var groupByColumns = map[string]string{
	"agent":     "agent",
	"team":      "team",
	"namespace": "namespace",
	"model":     "model",
	"provider":  "provider",
	// "deployment" groups by the workload column, which also holds StatefulSet
	// names; the user-facing name matches the common case.
	"deployment": "workload",
}

// ErrInvalidGroupBy marks a caller-supplied group_by outside the allowlist.
var ErrInvalidGroupBy = errors.New("invalid group_by")

type AggregateOptions struct {
	GroupBy string // one of groupByColumns; default "agent"
	// The window is inclusive on both ends (millisecond resolution):
	// a request recorded in the same millisecond as Until=now must count.
	Since time.Time
	Until time.Time
	Agent string // optional filter
	Model string // optional filter
}

type Row struct {
	Key                      string  `json:"key"`
	Requests                 int64   `json:"requests"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	CostUSD                  float64 `json:"cost_usd"`
}

func (s *Store) Aggregate(ctx context.Context, opts AggregateOptions) ([]Row, error) {
	if opts.GroupBy == "" {
		opts.GroupBy = "agent"
	}
	col, ok := groupByColumns[opts.GroupBy]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrInvalidGroupBy, opts.GroupBy)
	}

	query := `
		SELECT ` + col + `, COUNT(*),
			SUM(input_tokens), SUM(output_tokens),
			SUM(cache_creation_input_tokens), SUM(cache_read_input_tokens),
			SUM(cost_usd)
		FROM requests
		WHERE ts >= ? AND ts <= ?`
	args := []any{opts.Since.UnixMilli(), opts.Until.UnixMilli()}
	if opts.Agent != "" {
		query += " AND agent = ?"
		args = append(args, opts.Agent)
	}
	if opts.Model != "" {
		query += " AND model = ?"
		args = append(args, opts.Model)
	}
	query += ` GROUP BY ` + col + ` ORDER BY SUM(cost_usd) DESC, ` + col

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Row{}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Key, &r.Requests, &r.InputTokens, &r.OutputTokens,
			&r.CacheCreationInputTokens, &r.CacheReadInputTokens, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
