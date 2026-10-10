// Package provider defines shared invocation, channel runtime and management
// capability contracts. It contains no upstream credential interpretation.
package provider

import "errors"

// ErrUnsupported marks a provider capability that is not available.
var ErrUnsupported = errors.New("operation not supported by provider")
