package app

import (
	"context"
	"log"
	"sync"
	"time"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
)

type usageTimingKey struct{}
type usageTimingRows struct {
	mu          sync.Mutex
	ids         []int64
	pending     []store.UsageParams
	deferWrites bool
}

func trackUsageTiming(ctx context.Context, id int64) {
	if ctx == nil {
		return
	}
	if rows, ok := ctx.Value(usageTimingKey{}).(*usageTimingRows); ok {
		rows.mu.Lock()
		defer rows.mu.Unlock()
		if len(rows.ids) < 16 {
			rows.ids = append(rows.ids, id)
		}
	}
}

func (s *Server) persistUsageTiming(rows *usageTimingRows, t streamwatch.TimingSnapshot, status int, stream bool, elapsed ...time.Duration) {
	rows.mu.Lock()
	ids := append([]int64(nil), rows.ids...)
	rows.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	duration := time.Since(t.Start)
	if len(elapsed) > 0 {
		duration = elapsed[0]
	}
	value := store.UsagePerformance{HTTPStatus: status, ResponseStream: stream, RequestID: t.RequestID, QueueMS: t.Queue.Milliseconds(), AccountWaitMS: t.Account.Milliseconds(), TotalMS: duration.Milliseconds(), Attempts: t.Attempts, Upstream429: t.Upstream429}
	if t.BytesSeen {
		ms := t.FirstByte.Milliseconds()
		value.FirstByteMS = &ms
	}
	if t.GenerationEvents > 0 {
		first := t.FirstGeneration.Milliseconds()
		value.FirstGenerationMS = &first
		if t.GenerationEvents > 1 {
			ms := (t.LastGeneration - t.FirstGeneration).Milliseconds()
			if ms > 0 {
				value.GenerationMS = &ms
			}
		}
	}
	if t.Attempts > 0 {
		ms := max(int64(0), (duration - t.FirstAttempt - (t.Queue + t.Account - t.WaitAtAttempt)).Milliseconds())
		value.ExecutionMS = &ms
	}
	for _, stage := range t.AttemptStages {
		value.Stages = append(value.Stages, store.UsageAttempt{UpstreamRequestID: stage.UpstreamRequestID, Number: stage.Number, HeaderWaitMS: stage.HeaderWaitMS, HTTPStatus: stage.HTTPStatus, Finished: stage.Finished})
	}
	if err := s.o.db.UpdateUsagePerformance(ids, value); err != nil {
		log.Printf("保存调用性能诊断失败: %v", err)
	}
}
