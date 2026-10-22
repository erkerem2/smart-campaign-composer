package dispatch

import (
	"sync"
	"sync/atomic"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

// Run states reported to the dashboard.
const (
	StatePlanning    = "planning"    // resolving audience, claiming deliveries
	StateSending     = "sending"     // workers are delivering
	StateCompleted   = "completed"   // every delivery reached sent or failed
	StateFailed      = "failed"      // nothing could be delivered, or a fatal error
	StateInterrupted = "interrupted" // stopped (shutdown); campaign stays 'sending' and can be resumed
	StateIdle        = "idle"        // no run in this process; numbers come from the database
)

// ChannelStats is the progress of one channel.
type ChannelStats struct {
	Total     int64 `json:"total"`     // deliveries to make (opted-in, not sent before)
	Processed int64 `json:"processed"` // reached a final state: sent + failed
	Sent      int64 `json:"sent"`
	Failed    int64 `json:"failed"`
	Retried   int64 `json:"retried"` // retry attempts (a message retried twice counts 2)
	Pending   int64 `json:"pending"` // total - processed
	OptedOut  int64 `json:"opted_out"`
	Skipped   int64 `json:"skipped"` // already dispatched earlier / duplicate in audience
}

// Stats is the live state of one campaign's dispatch.
type Stats struct {
	CampaignID int64                           `json:"campaign_id"`
	State      string                          `json:"state"`
	Live       bool                            `json:"live"` // true while a run is active in this process
	Total      int64                           `json:"total"`
	Processed  int64                           `json:"processed"`
	Sent       int64                           `json:"sent"`
	Failed     int64                           `json:"failed"`
	Retried    int64                           `json:"retried"`
	Pending    int64                           `json:"pending"`
	OptedOut   int64                           `json:"opted_out"`
	Skipped    int64                           `json:"skipped"`
	Channels   map[domain.Channel]ChannelStats `json:"channels"`
	StartedAt  *time.Time                      `json:"started_at,omitempty"`
	FinishedAt *time.Time                      `json:"finished_at,omitempty"`
	Error      string                          `json:"error,omitempty"`
}

// Final reports whether the run will not change any more.
func (s Stats) Final() bool {
	return s.State == StateCompleted || s.State == StateFailed || s.State == StateInterrupted || s.State == StateIdle
}

type channelCounters struct {
	total, sent, failed, retried, optedOut, skipped atomic.Int64
}

// progress holds lock-free counters updated by workers and read by the
// dashboard at any time.
type progress struct {
	campaignID int64
	channels   map[domain.Channel]*channelCounters // fixed key set, read-only map

	mu       sync.Mutex
	state    string
	started  time.Time
	finished time.Time
	err      string
}

func newProgress(campaignID int64, started time.Time) *progress {
	p := &progress{campaignID: campaignID, channels: map[domain.Channel]*channelCounters{},
		state: StatePlanning, started: started}
	for _, ch := range domain.Channels {
		p.channels[ch] = &channelCounters{}
	}
	return p
}

func (p *progress) setState(state string, at time.Time, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = state
	if state != StatePlanning && state != StateSending {
		p.finished = at
	}
	if err != nil {
		p.err = err.Error()
	}
}

func (p *progress) snapshot(live bool) Stats {
	p.mu.Lock()
	s := Stats{CampaignID: p.campaignID, State: p.state, Live: live, Error: p.err,
		Channels: make(map[domain.Channel]ChannelStats, len(p.channels))}
	started := p.started
	s.StartedAt = &started
	if !p.finished.IsZero() {
		f := p.finished
		s.FinishedAt = &f
	}
	p.mu.Unlock()

	for ch, c := range p.channels {
		cs := ChannelStats{
			Total: c.total.Load(), Sent: c.sent.Load(), Failed: c.failed.Load(),
			Retried: c.retried.Load(), OptedOut: c.optedOut.Load(), Skipped: c.skipped.Load(),
		}
		cs.Processed = cs.Sent + cs.Failed
		cs.Pending = cs.Total - cs.Processed
		s.Channels[ch] = cs
		s.add(cs)
	}
	return s
}

func (s *Stats) add(c ChannelStats) {
	s.Total += c.Total
	s.Processed += c.Processed
	s.Sent += c.Sent
	s.Failed += c.Failed
	s.Retried += c.Retried
	s.Pending += c.Pending
	s.OptedOut += c.OptedOut
	s.Skipped += c.Skipped
}

// statsFromCounts builds Stats from persisted dispatch_logs, e.g. after a
// restart. Opt-outs and skips are not persisted and are reported as 0.
func statsFromCounts(campaignID int64, status domain.CampaignStatus, counts map[domain.Channel]domain.DispatchCounts) Stats {
	s := Stats{CampaignID: campaignID, State: StateIdle, Channels: map[domain.Channel]ChannelStats{}}
	switch status {
	case domain.CampaignStatusCompleted:
		s.State = StateCompleted
	case domain.CampaignStatusFailed:
		s.State = StateFailed
	case domain.CampaignStatusSending:
		// Sending in the database but no run here: interrupted, resumable.
		s.State = StateInterrupted
	}
	for _, ch := range domain.Channels {
		c := counts[ch]
		cs := ChannelStats{Total: c.Total, Sent: c.Sent, Failed: c.Failed, Retried: c.Retries,
			Processed: c.Sent + c.Failed, Pending: c.Pending}
		s.Channels[ch] = cs
		s.add(cs)
	}
	return s
}
