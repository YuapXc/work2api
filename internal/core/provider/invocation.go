package provider

import (
	"context"
	"encoding/json"
)

// DispatchError is a safe client-facing failure produced before upstream I/O.
type DispatchError struct {
	Status        int
	Message, Type string
}

func (e *DispatchError) Error() string { return e.Message }

// AccountRef keeps identities from different credential sources distinct.
// LocalID remains unchanged for legacy storage and public account controls.
type AccountRef struct{ Provider, LocalID string }

// Caller contains stable authenticated identity, never the raw API key.
type Caller struct {
	AppID, UserID int64
	AppName       string
	AccountScope  map[string]bool
}

// Invocation carries core-owned checks into provider code. Check is invoked
// immediately before dispatch, including after credentials or queue waits.
// It does not grant permission merely because an earlier check succeeded.
type Invocation struct {
	Provider      string
	UsageObserver interface{ Observe(map[string]any) }
	Select        func(context.Context, func(map[string]bool) (AccountRef, error)) (AccountRef, error)
	Caller        Caller
	Check         func(context.Context, AccountRef, string) error
	Wait          func(context.Context, func(context.Context) error) error
}

type invocationKey struct{}

func WithInvocation(ctx context.Context, call Invocation) context.Context {
	return context.WithValue(ctx, invocationKey{}, call)
}

func CurrentInvocation(ctx context.Context) Invocation {
	if ctx == nil {
		return Invocation{}
	}
	call, _ := ctx.Value(invocationKey{}).(Invocation)
	return call
}

func CheckDispatch(ctx context.Context, account AccountRef, model string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check := CurrentInvocation(ctx).Check; check != nil {
		return check(ctx, account, model)
	}
	return nil
}

func Wait(ctx context.Context, wait func(context.Context) error) error {
	if coordinator := CurrentInvocation(ctx).Wait; coordinator != nil {
		return coordinator(ctx, wait)
	}
	return wait(ctx)
}

func (c *Caller) SessionCaller() Caller {
	if c == nil {
		return Caller{}
	}
	return *c
}

// ResourceKey is unambiguous even when channels reuse a local account identifier.
func (a AccountRef) ResourceKey() string {
	data, _ := json.Marshal([2]string{a.Provider, a.LocalID})
	return string(data)
}
func LocalBusy(busy map[string]bool, name string) map[string]bool {
	out := map[string]bool{}
	for key, value := range busy {
		var ref [2]string
		if json.Unmarshal([]byte(key), &ref) == nil && ref[0] == name {
			out[ref[1]] = value
		}
	}
	return out
}
func DisplayAccountCounts(counts map[string]int, defaultName string) map[string]int {
	out := map[string]int{}
	for key, n := range counts {
		var ref [2]string
		if json.Unmarshal([]byte(key), &ref) == nil {
			label := ref[1]
			if ref[0] != defaultName {
				label = ref[0] + "/" + ref[1]
			}
			out[label] = n
		}
	}
	return out
}
