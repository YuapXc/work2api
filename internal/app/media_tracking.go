package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
	"work2api/internal/core/provider"
	"work2api/internal/statebackup"
	"work2api/internal/store"
)

type mediaCheckFlight struct {
	done   chan struct{}
	output map[string]any
	err    error
}
type mediaCheckFlights struct {
	mu     sync.Mutex
	active map[string]*mediaCheckFlight
}

func (o *Orchestrator) validateMediaTask(ctx context.Context, t *store.MediaTask) (*Principal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := &Principal{}
	if t.AppID > 0 {
		hash, err := o.db.GetAppKeyHash(t.AppID)
		if err != nil {
			return nil, errBody(403, "任务密钥已撤销", "auth_error")
		}
		app, err := o.db.FindAppByKey(hash)
		if err != nil {
			return nil, err
		}
		if app == nil {
			return nil, errBody(403, "任务密钥已停用", "auth_error")
		}
		var ae *apiError
		p, ae = o.principalForApp(app)
		if ae != nil {
			return nil, ae
		}
	}
	if p.UserID != t.UserID {
		return nil, errBody(403, "任务归属已改变", "permission_error")
	}
	requested := t.RequestModel
	if requested == "" {
		requested = t.Model
	}
	s := &Server{o: o}
	if ae := s.prepareModel(p, map[string]any{"model": requested}); ae != nil {
		return nil, ae
	}
	if p.EffectiveModel != t.Model || p.RouteModel != "" || p.UserID > 0 && !p.AccountScope[t.AccountUID] {
		return nil, errBody(403, "任务模型或账号权限已改变", "model_not_allowed")
	}
	if p.UserID == 0 {
		excluded, err := o.db.PrivatePoolExcludedUIDs()
		if err != nil {
			return nil, err
		}
		if excluded[t.AccountUID] {
			return nil, errBody(403, "原任务账号已撤回共享", "account_not_allowed")
		}
	}
	return p, nil
}

func (o *Orchestrator) queryMediaTask(ctx context.Context, id string) (map[string]any, error) {
	o.mediaChecks.mu.Lock()
	if o.mediaChecks.active == nil {
		o.mediaChecks.active = map[string]*mediaCheckFlight{}
	}
	if flight := o.mediaChecks.active[id]; flight != nil {
		o.mediaChecks.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			return flight.output, flight.err
		}
	}
	flight := &mediaCheckFlight{done: make(chan struct{})}
	o.mediaChecks.active[id] = flight
	o.mediaChecks.mu.Unlock()
	defer func() {
		o.mediaChecks.mu.Lock()
		delete(o.mediaChecks.active, id)
		close(flight.done)
		o.mediaChecks.mu.Unlock()
	}()
	flight.output, flight.err = o.fetchMediaTask(ctx, id)
	return flight.output, flight.err
}

func (o *Orchestrator) fetchMediaTask(ctx context.Context, id string) (map[string]any, error) {
	t, err := o.db.MediaTask(id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errBody(404, "任务不存在", "not_found")
	}
	p, err := o.validateMediaTask(ctx, t)
	if err != nil {
		return nil, err
	}
	if t.Status == "completed" || t.Status == "failed" || t.Status == "cancelled" || t.UpstreamID == "" {
		return t.Response, nil
	}
	rt, ok := o.runtimes.Resolve(t.Model)
	if !ok || rt.Name() != t.Provider {
		return nil, errBody(503, "原任务渠道不可用", "upstream_error")
	}
	media, ok := rt.(provider.MediaProvider)
	if !ok {
		return nil, provider.ErrUnsupported
	}
	call := provider.CurrentInvocation(ctx)
	call.Provider = t.Provider
	call.Caller = p.SessionCaller()
	inherited := call.Check
	call.Check = func(c context.Context, ref provider.AccountRef, model string) error {
		if inherited != nil {
			if err := inherited(c, ref, model); err != nil {
				return err
			}
		}
		if ref.Provider != t.Provider || ref.LocalID != t.AccountUID {
			return errBody(403, "任务账号不可替换", "permission_error")
		}
		_, err := o.validateMediaTask(c, t)
		return err
	}
	ctx = provider.WithInvocation(ctx, call)
	operation := "video-query"
	if t.Kind == "audio" {
		operation = "transcription-query"
	}
	result, err := media.CallMedia(ctx, provider.MediaRequest{Operation: operation, Model: physicalModel(t.Model), AccountID: t.AccountUID, Payload: map[string]any{"task_id": t.UpstreamID}})
	if err != nil {
		return nil, err
	}
	status, _ := result["status"].(string)
	if status == "pending" {
		status = "queued"
	}
	if status == "processing" {
		status = "in_progress"
	}
	if status == "" {
		status = "unknown"
	}
	output := map[string]any{"id": t.ID, "object": t.Kind, "model": t.Model, "status": status, "created_at": int64(t.CreatedAt), "can_poll": status != "completed" && status != "failed" && status != "cancelled"}
	if status == "completed" && t.Kind == "audio" {
		transcript, _ := result["audio_transcription"].(map[string]any)
		output["text"] = transcript["text"]
	}
	if status == "completed" && t.Kind == "video" {
		items, ok := result["videos"].([]any)
		if !ok {
			items, _ = result["data"].([]any)
		}
		if len(items) == 0 {
			return nil, errBody(502, "任务已完成但尚无视频产物，请稍后查询原任务", "media_result_unavailable")
		}
		output["data"] = items
	}
	if v, ok := result["usage"]; ok {
		output["usage"] = v
	}
	message := ""
	if status == "failed" || status == "cancelled" {
		message = "上游媒体任务" + status
		output["error"] = map[string]any{"message": message}
		for _, key := range []string{"error", "message", "msg", "reason", "fail_reason"} {
			if v, ok := result[key]; ok {
				output[key] = v
			}
		}
	}

	if err = o.db.UpdateMediaTask(t.ID, t.UpstreamID, status, output, mediaCredit(result), message); err != nil {
		return nil, errBody(503, "任务状态保存失败，可继续查询原任务", "server_error")
	}
	saved, err := o.db.MediaTask(id)
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, errBody(404, "任务不存在", "not_found")
	}
	return saved.Response, nil
}

func (s *Scheduler) runMediaTracking(done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if s.context().Err() != nil {
			return
		}
		s.reconcileMediaTasks(s.context())
		select {
		case <-s.context().Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) reconcileMediaTasks(ctx context.Context) {
	ids, err := s.o.db.PendingMediaTasks(float64(time.Now().UnixNano()) / 1e9)
	if err != nil {
		return
	}
	jobs := make(chan string, len(ids))
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	var group sync.WaitGroup
	for i := 0; i < 2 && i < len(ids); i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					return
				}
				leave := statebackup.Enter()
				func() {
					defer leave()
					attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
					defer cancel()
					_, err := s.o.queryMediaTask(attempt, id)
					if err != nil && ctx.Err() == nil {
						t, loadErr := s.o.db.MediaTask(id)
						if loadErr != nil || t == nil {
							return
						}
						delay := 15 * time.Second * time.Duration(1<<min(t.CheckFailures, 6))
						message := "任务查询暂未成功，后台稍后重试"
						var ae *provider.APIError
						if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
							delay = 15 * time.Minute
							message = "授权或权限不可用，后台查询等待恢复"
						}
						_ = s.o.db.DeferMediaTaskCheck(id, message, time.Now().Add(delay))
					}
				}()
			}
		}()
	}
	group.Wait()
}
