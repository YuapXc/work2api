package store

import "encoding/json"

type UsageAttempt struct {
	Number       int   `json:"number"`
	HeaderWaitMS int64 `json:"header_wait_ms"`
	HTTPStatus   int   `json:"http_status"`
	Finished     bool  `json:"headers_finished"`
}

// Whole-request timing shared by retry rows, with no content or account IDs.
type UsagePerformance struct {
	RequestID     string         `json:"request_id"`
	QueueMS       int64          `json:"queue_ms"`
	AccountWaitMS int64          `json:"account_wait_ms"`
	TotalMS       int64          `json:"total_ms"`
	FirstByteMS   *int64         `json:"first_byte_ms,omitempty"`
	ExecutionMS   *int64         `json:"execution_ms,omitempty"`
	Attempts      int            `json:"attempts"`
	Upstream429   int            `json:"upstream_429"`
	Stages        []UsageAttempt `json:"attempt_stages,omitempty"`
}

// Target only IDs inserted by this request; preserve existing protocol facts.
func (d *DB) UpdateUsagePerformance(ids []int64, value UsagePerformance) error {
	if len(ids) == 0 {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE usage_logs SET diagnostics=json_set(COALESCE(diagnostics,'{}'),'$.performance',json(?)) WHERE id=?`, string(raw), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
