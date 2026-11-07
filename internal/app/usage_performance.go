package app

import (
	"sync"
	"time"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
)

type usageTimingKey struct{}
type usageTimingRows struct {
	mu      sync.Mutex
	pending []store.UsageParams
}

func (s *Server) persistUsageTiming(rows *usageTimingRows, t streamwatch.TimingSnapshot, status int, stream bool, duration time.Duration) {
	rows.mu.Lock()
	pending := rows.pending
	rows.pending = nil
	rows.mu.Unlock()
	if len(pending) == 0 {
		return
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
	for _, params := range pending {
		if params.Diagnostics == nil {
			params.Diagnostics = &store.UsageDiagnostics{}
		}
		params.Diagnostics.Performance = &value
		s.o.persistUsage(params)
	}
}
