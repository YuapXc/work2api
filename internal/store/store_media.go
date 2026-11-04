package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

type MediaTask struct {
	ID, Provider, AccountUID, Model, RequestModel, Kind, UpstreamID, Status string
	Metadata                                                                map[string]any
	AppID, UserID, UsageID                                                  int64
	CreatedAt, UpdatedAt                                                    float64
	Response                                                                map[string]any
	NextCheckAt                                                             float64
	CheckFailures                                                           int
	CheckError                                                              string
}

func (d *DB) CreateMediaTask(t MediaTask, appName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := float64(time.Now().UnixNano()) / 1e9
	diag, _ := json.Marshal(map[string]any{"media_task_id": t.ID, "finish_reason": "submitting", "media": t.Metadata})
	res, err := tx.Exec(`INSERT INTO usage_logs(ts,model,protocol,account_uid,input_tokens,output_tokens,total_tokens,latency_ms,status,error,input_content,output_content,reasoning_content,app_name,user_id,app_id,tokens_known,diagnostics,content_bytes) VALUES(?,?,?,?,0,0,0,0,'incomplete','','','','',?,?,?,0,?,0)`, now, t.Model, t.Kind, t.AccountUID, appName, nullableID(t.UserID), nullableID(t.AppID), string(diag))
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO media_tasks(id,provider,account_uid,app_id,user_id,model,kind,requested_model,status,created_at,updated_at,usage_id) VALUES(?,?,?,?,?,?,?,?,'submitting',?,?,?)`, t.ID, t.Provider, t.AccountUID, t.AppID, t.UserID, t.Model, t.Kind, t.RequestModel, now, now, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) MediaTask(id string) (*MediaTask, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := &MediaTask{}
	var raw string
	err := d.db.QueryRow(`SELECT id,provider,account_uid,app_id,user_id,model,kind,requested_model,upstream_id,status,response,created_at,updated_at,usage_id,next_check_at,check_failures,check_error FROM media_tasks WHERE id=?`, id).Scan(&t.ID, &t.Provider, &t.AccountUID, &t.AppID, &t.UserID, &t.Model, &t.Kind, &t.RequestModel, &t.UpstreamID, &t.Status, &raw, &t.CreatedAt, &t.UpdatedAt, &t.UsageID, &t.NextCheckAt, &t.CheckFailures, &t.CheckError)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal([]byte(raw), &t.Response)
	return t, err
}

// UpdateMediaTask and its usage row commit together. Repeated terminal polls cannot charge or log twice.
func (d *DB) UpdateMediaTask(id, upstreamID, status string, response map[string]any, credits *float64, message string) error {
	raw, err := json.Marshal(response)
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
	var usageID int64
	var created float64
	var previous, previousDiagnostics string
	if err = tx.QueryRow(`SELECT t.usage_id,t.created_at,t.status,COALESCE(u.diagnostics,'{}') FROM media_tasks t LEFT JOIN usage_logs u ON u.id=t.usage_id WHERE t.id=?`, id).Scan(&usageID, &created, &previous, &previousDiagnostics); err != nil {
		return err
	}
	if previous == "completed" || previous == "failed" || previous == "cancelled" {
		return nil
	}
	now := float64(time.Now().UnixNano()) / 1e9
	_, err = tx.Exec(`UPDATE media_tasks SET upstream_id=CASE WHEN ?='' THEN upstream_id ELSE ? END,status=?,response=?,updated_at=?,next_check_at=?,check_failures=0,check_error='' WHERE id=?`, upstreamID, upstreamID, status, string(raw), now, now+10, id)
	if err != nil {
		return err
	}
	state := "incomplete"
	if status == "completed" {
		state = "ok"
	}
	if status == "failed" || status == "cancelled" {
		state = "error"
	}
	var cost any
	if credits != nil {
		cost = *credits
	}
	details := map[string]any{}
	_ = json.Unmarshal([]byte(previousDiagnostics), &details)
	details["media_task_id"] = id
	details["finish_reason"] = status
	if usage, ok := response["usage"].(map[string]any); ok {
		if metadata, ok := details["media"].(map[string]any); ok {
			for _, key := range []string{"output_video_duration", "output_video_coefficient"} {
				if value, exists := usage[key]; exists {
					metadata[key] = value
				}
			}
		}
	}
	diag, _ := json.Marshal(details)
	_, err = tx.Exec(`UPDATE usage_logs SET status=?,error=?,credits=COALESCE(?,credits),latency_ms=?,diagnostics=? WHERE id=?`, state, message, cost, (now-created)*1000, string(diag), usageID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) MediaTaskList(appID, userID int64, before float64) ([]map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if before <= 0 {
		before = float64(time.Now().UnixNano())/1e9 + 1
	}
	rows, err := d.db.Query(`SELECT id,model,requested_model,kind,status,created_at,updated_at,check_error,next_check_at,upstream_id<>'' FROM media_tasks WHERE app_id=? AND user_id=? AND created_at<? ORDER BY created_at DESC,id DESC LIMIT 50`, appID, userID, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, model, requested, kind, status string
		var created, updated, nextCheck float64
		var checkError string
		var submitted bool
		if err := rows.Scan(&id, &model, &requested, &kind, &status, &created, &updated, &checkError, &nextCheck, &submitted); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "model": model, "requested_model": requested, "kind": kind, "status": status, "created_at": created, "updated_at": updated, "check_message": checkError, "next_check_at": nextCheck, "can_poll": submitted && status != "completed" && status != "failed" && status != "cancelled"})
	}
	return out, rows.Err()
}

// PendingMediaTasks selects only due, submitted tasks; generation is never retried.
func (d *DB) PendingMediaTasks(now float64) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(`SELECT id FROM media_tasks WHERE upstream_id<>'' AND status NOT IN ('completed','failed','cancelled') AND next_check_at<=? ORDER BY next_check_at,updated_at LIMIT 16`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (d *DB) DeferMediaTaskCheck(id, message string, after time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`UPDATE media_tasks SET next_check_at=?,check_failures=check_failures+1,check_error=? WHERE id=? AND status NOT IN ('completed','failed','cancelled')`, float64(after.UnixNano())/1e9, message, id)
	return err
}
