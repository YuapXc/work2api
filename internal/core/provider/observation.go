package provider

import (
	"math"
	"sort"
	"sync"
	"time"
)

// NativeSessionObserver marks channels that already maintain richer observations
// and controls. The common observer never overrides their affinity policy.
type NativeSessionObserver interface{ OwnsSessionObservations() bool }

type ObservedSession struct {
	ID       string  `json:"id"`
	Provider string  `json:"-"`
	Model    string  `json:"model"`
	App      string  `json:"app"`
	UserID   int64   `json:"user_id"`
	Source   string  `json:"source"`
	Started  float64 `json:"started_at"`
	Last     float64 `json:"last_at"`
	Requests int     `json:"requests"`
	Running  int     `json:"running"`
	Credits  float64 `json:"credits"`
	Known    int     `json:"credits_known"`
	Unknown  int     `json:"credits_unknown"`
	UID      string  `json:"account_uid"`
}

// SessionObservations is bounded, memory-only telemetry. Only explicit session
// signals are observed: request IDs, prompts and credentials are never stored.
// Missing account identity is left unknown instead of claiming a binding.
type SessionObservations struct {
	mu   sync.Mutex
	rows map[string]*ObservedSession
}

func (o *SessionObservations) prune(now float64) {
	for id, row := range o.rows {
		if row.Running == 0 && now-row.Last > 1800 {
			delete(o.rows, id)
		}
	}
}

func (o *SessionObservations) Begin(channel string, req ServeRequest) func(UsageReport) {
	signal := ExplicitSessionID(req.Headers, req.Payload)
	if signal == "" {
		return func(UsageReport) {}
	}
	model, _ := req.Payload["model"].(string)
	id := ScopedSessionID(req.Caller, req.Headers, channel, model, signal)
	now := float64(time.Now().UnixMilli()) / 1000
	o.mu.Lock()
	if o.rows == nil {
		o.rows = map[string]*ObservedSession{}
	}
	o.prune(now)
	row := o.rows[id]
	if row == nil {
		if len(o.rows) >= 2000 {
			var oldest *ObservedSession
			for _, v := range o.rows {
				if v.Running == 0 && (oldest == nil || v.Last < oldest.Last) {
					oldest = v
				}
			}
			if oldest == nil {
				o.mu.Unlock()
				return func(UsageReport) {}
			}
			delete(o.rows, oldest.ID)
		}
		row = &ObservedSession{ID: id, Provider: channel, Model: model, App: req.AppName, UserID: req.Caller.UserID, Source: "sid", Started: now}
		o.rows[id] = row
	}
	row.Last = now
	row.Requests++
	row.Running++
	o.mu.Unlock()
	var once sync.Once
	return func(report UsageReport) {
		once.Do(func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			row.Running--
			row.Last = float64(time.Now().UnixMilli()) / 1000
			row.UID = report.AccountUID
			if report.Credits != nil && !math.IsNaN(*report.Credits) && !math.IsInf(*report.Credits, 0) && *report.Credits >= 0 {
				row.Credits += *report.Credits
				row.Known++
			} else {
				row.Unknown++
			}
		})
	}
}

func (o *SessionObservations) Views() []ObservedSession {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.prune(float64(time.Now().UnixMilli()) / 1000)
	rows := make([]ObservedSession, 0, len(o.rows))
	for _, row := range o.rows {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Last > rows[j].Last })
	return rows
}
