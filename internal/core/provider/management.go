package provider

import "math"

// ResourceSummary is a credential-free management projection. Channels retain
// their detailed rows and dispatch policies; actions describe this resource,
// not every resource belonging to the same channel.
type ResourceSummary struct {
	ID      string                    `json:"id"`
	Kind    string                    `json:"kind"`
	Label   string                    `json:"label"`
	Source  string                    `json:"source,omitempty"`
	Region  string                    `json:"region,omitempty"`
	Status  string                    `json:"status"`
	Quota   ResourceQuota             `json:"quota"`
	Actions map[string]ResourceAction `json:"actions"`
}

// Missing values mean unknown. Balances belong to their own channel and must
// not be added across upstreams with different billing rules.
type ResourceQuota struct {
	Remaining     *float64 `json:"remaining"`
	Total         *float64 `json:"total"`
	ExpiresAt     *float64 `json:"expires_at"`
	Stale         bool     `json:"stale"`
	RefreshFailed bool     `json:"refresh_failed"`
}

func resourceNumber(value any) *float64 {
	var n float64
	switch v := value.(type) {
	case float64:
		n = v
	case int64:
		n = float64(v)
	case int:
		n = float64(v)
	case *float64:
		if v == nil {
			return nil
		}
		n = *v
	default:
		return nil
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}

type ResourceAction struct {
	Label        string `json:"label"`
	Reason       string `json:"reason,omitempty"`
	Enabled      bool   `json:"enabled"`
	Value        string `json:"value,omitempty"`
	Confirmation string `json:"confirmation,omitempty"`
}

// MaintenanceOption describes an existing scheduler setting owned by a channel.
// Keys remain validated by the application's settings endpoint.
type MaintenanceOption struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
}

// Small optional contracts let channels expose independent operations.
type AccountRenamer interface{ RenameAccount(string, string) error }
type AccountActivator interface{ ActivateAccount(string) error }
type AccountDeleter interface{ DeleteAccount(string) error }
type AccountEnabler interface{ SetAccountEnabled(string, bool) error }
type AccountPrioritizer interface{ SetAccountPriority(string, int) error }

// ResourceMetadata projects local admin rows without exposing arbitrary fields.
func ResourceMetadata(row map[string]any, kind string, actions map[string]ResourceAction) ResourceSummary {
	str := func(key string) string { s, _ := row[key].(string); return s }
	id := str("id")
	if id == "" {
		id = str("uid")
	}
	label := str("label")
	if label == "" {
		label = str("alias")
	}
	if label == "" {
		label = str("nickname")
	}
	if label == "" {
		label = id
	}
	region := str("region")
	if region == "" {
		region = str("site")
	}
	status := "unknown"
	switch {
	case row["enabled"] == false:
		status = "disabled"
	case kind == "key_tier" || kind == "anonymous":
		status = "unconfigured"
		if n := resourceNumber(row["key_count"]); n != nil && *n > 0 {
			status = "configured"
		}
	case row["quota_exceeded"] == true:
		status = "quota_exceeded"
	case row["healthy"] == true:
		status = "healthy"
	case row["has_secret"] == false:
		status = "unauthorized"
	case row["healthy"] == false:
		status = "unhealthy"
	}
	return ResourceSummary{ID: id, Kind: kind, Label: label, Source: str("source"), Region: region, Status: status, Actions: actions,
		Quota: ResourceQuota{Remaining: resourceNumber(row["credits_remaining"]), Total: resourceNumber(row["credits_total"]), ExpiresAt: resourceNumber(row["credits_expire_at"]), Stale: row["quota_stale"] == true, RefreshFailed: row["quota_refresh_failed"] == true}}
}
