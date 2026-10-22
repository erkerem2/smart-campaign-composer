package segment

import (
	"context"
	"database/sql"
	"fmt"
)

// AudienceCounts is the result of an audience preview query.
type AudienceCounts struct {
	Total      int64 `json:"total"`
	SMSOptIn   int64 `json:"sms_opt_in"`
	EmailOptIn int64 `json:"email_opt_in"`
	Reachable  int64 `json:"reachable"` // opted in to at least one channel
}

// AudienceCounter counts users matching a validated rule.
type AudienceCounter struct {
	db      *sql.DB
	reg     *Registry
	builder *Builder
}

func NewAudienceCounter(db *sql.DB, reg *Registry, d Dialect) *AudienceCounter {
	return &AudienceCounter{db: db, reg: reg, builder: NewBuilder(reg, d)}
}

// Query returns the full aggregate SQL and its bind args (useful for logging
// and EXPLAIN). Table/column names come only from the validated Registry.
func (c *AudienceCounter) Query(rule *Node) (string, []any, error) {
	w, err := c.builder.Build(rule)
	if err != nil {
		return "", nil, err
	}
	sms, email := c.reg.SMSColumn, c.reg.EmailCol
	q := fmt.Sprintf(
		"SELECT COUNT(*), "+
			"COALESCE(SUM(CASE WHEN %[1]s THEN 1 ELSE 0 END), 0), "+
			"COALESCE(SUM(CASE WHEN %[2]s THEN 1 ELSE 0 END), 0), "+
			"COALESCE(SUM(CASE WHEN %[1]s OR %[2]s THEN 1 ELSE 0 END), 0) "+
			"FROM %[3]s WHERE %[4]s",
		sms, email, c.reg.Table, w.SQL)
	return q, w.Args, nil
}

// Count runs a single aggregate query (one round-trip, one table scan).
func (c *AudienceCounter) Count(ctx context.Context, rule *Node) (AudienceCounts, error) {
	q, args, err := c.Query(rule)
	if err != nil {
		return AudienceCounts{}, err
	}
	var r AudienceCounts
	if err := c.db.QueryRowContext(ctx, q, args...).Scan(&r.Total, &r.SMSOptIn, &r.EmailOptIn, &r.Reachable); err != nil {
		return AudienceCounts{}, fmt.Errorf("segment: audience query: %w", err)
	}
	return r, nil
}
