package provider

import "context"

type MediaRequest struct {
	Operation string
	Model     string
	AccountID string
	Payload   map[string]any
}

type MediaProvider interface {
	PrepareMedia(context.Context, string, map[string]any, Caller) (MediaRequest, error)
	CallMedia(context.Context, MediaRequest) (map[string]any, error)
}

type MediaUploader interface {
	UploadMedia(context.Context, MediaRequest, map[string]any) (string, error)
}
