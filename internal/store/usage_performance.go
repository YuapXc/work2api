package store

type UsageAttempt struct {
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	Number            int    `json:"number"`
	HeaderWaitMS      int64  `json:"header_wait_ms"`
	HTTPStatus        int    `json:"http_status"`
	Finished          bool   `json:"headers_finished"`
}

// Whole-request timing shared by retry rows, with no content or account IDs.
type UsagePerformance struct {
	HTTPStatus        int            `json:"http_status,omitempty"`
	ResponseStream    bool           `json:"response_stream"`
	RequestID         string         `json:"request_id"`
	QueueMS           int64          `json:"queue_ms"`
	AccountWaitMS     int64          `json:"account_wait_ms"`
	TotalMS           int64          `json:"total_ms"`
	FirstByteMS       *int64         `json:"first_byte_ms,omitempty"`
	FirstGenerationMS *int64         `json:"first_generation_ms,omitempty"`
	GenerationMS      *int64         `json:"generation_ms,omitempty"`
	ExecutionMS       *int64         `json:"execution_ms,omitempty"`
	Attempts          int            `json:"attempts"`
	Upstream429       int            `json:"upstream_429"`
	Stages            []UsageAttempt `json:"attempt_stages,omitempty"`
}
