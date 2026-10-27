package repository

import (
	"context"
	"database/sql"
	"fmt"

	"smart-campaign-composer/backend/internal/domain"
)

// MetricsRepository answers system-wide aggregate queries for GET /metrics.
type MetricsRepository struct {
	db *sql.DB
}

// NewMetricsRepository returns a repository backed by db.
func NewMetricsRepository(db *sql.DB) *MetricsRepository {
	return &MetricsRepository{db: db}
}

// Totals counts every campaign and aggregates every dispatch_logs row in a
// single read (one snapshot, so the numbers are consistent with each other).
//
// The numbers are durable: they survive restarts and include runs of other
// processes. A delivery is counted as sent or failed as soon as its row is
// finalised; its retries are recorded at the same moment, so retries of
// deliveries still in flight are not included yet.
func (r *MetricsRepository) Totals(ctx context.Context) (domain.SystemTotals, error) {
	var t domain.SystemTotals
	err := r.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM campaigns),
		       COALESCE(SUM(CASE WHEN status = 'sent'   THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(retry_count), 0)
		FROM dispatch_logs`).Scan(&t.Campaigns, &t.Sent, &t.Failed, &t.Retries)
	if err != nil {
		return domain.SystemTotals{}, fmt.Errorf("metrics totals: %w", err)
	}
	return t, nil
}
