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
	cost_usd REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_agent_ts ON requests(agent, ts);
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
	return &Store{db: db}, nil
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
}

func (s *Store) Insert(ctx context.Context, r Record) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO requests (
			ts, agent, team, namespace, provider, model, method, path,
			status, duration_ms, stream,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens, cost_usd
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Time.UnixMilli(), r.Agent, r.Team, r.Namespace, r.Provider, r.Model,
		r.Method, r.Path, r.Status, r.DurationMS, r.Stream,
		r.Usage.InputTokens, r.Usage.OutputTokens,
		r.Usage.CacheCreationInputTokens, r.Usage.CacheReadInputTokens, r.CostUSD,
	)
	return err
}

// groupByColumns is the allowlist for GET /usage grouping; the map value is
// interpolated into SQL, so only vetted column names may appear here.
var groupByColumns = map[string]string{
	"agent":     "agent",
	"team":      "team",
	"namespace": "namespace",
	"model":     "model",
	"provider":  "provider",
}

// ErrInvalidGroupBy marks a caller-supplied group_by outside the allowlist.
var ErrInvalidGroupBy = errors.New("invalid group_by")

type AggregateOptions struct {
	GroupBy string // one of groupByColumns; default "agent"
	Since   time.Time
	Until   time.Time
	Agent   string // optional filter
	Model   string // optional filter
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
		WHERE ts >= ? AND ts < ?`
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
