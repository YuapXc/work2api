package checkin

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"work2api/internal/qoder/account"
)

type statsTestTransport func(*http.Request) (*http.Response, error)

func (f statsTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOptionalStatsTimeoutPreservesClaim(t *testing.T) {
	previous := checkinClient
	t.Cleanup(func() { checkinClient = previous })
	for _, mode := range []string{"timeout", "cancel", "already-canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			checkinClient = &http.Client{Transport: statsTestTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				deadline, ok := req.Context().Deadline()
				if !ok || time.Until(deadline) > 3*time.Second {
					t.Error("optional statistics lack short deadline")
				}
				if mode == "cancel" {
					cancel()
				}
				<-req.Context().Done()
				return nil, req.Context().Err()
			})}
			if mode == "already-canceled" {
				cancel()
			}
			result := CheckinResult{Status: checkinStatusClaimed, Amount: 100, Message: "claimed", StreakDays: 5}
			before := result
			started := time.Now()
			readDailyCheckinStats(ctx, "fixture", &result)
			if result != before {
				t.Fatal("optional failure changed claim", result)
			}
			want := 1
			if mode == "already-canceled" {
				want = 0
			}
			if calls != want {
				t.Fatal("unexpected optional requests", calls)
			}
			if mode != "timeout" && time.Since(started) > time.Second {
				t.Fatal("parent cancellation did not stop statistics")
			}
		})
	}
}

func TestCheckinBatchBoundedOrderedAndCanceled(t *testing.T) {
	previous := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(previous) })
	accounts := make([]account.Account, 7)
	for i := range accounts {
		accounts[i] = account.Account{ID: fmt.Sprint(i)}
	}
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancel), func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			gate := make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(gate) })
			started := make(chan struct{}, len(accounts))
			var running, maximum atomic.Int32
			done := make(chan []CheckinResult, 1)
			go func() {
				done <- checkinBatch(ctx, accounts, func(ctx context.Context, acct *account.Account) CheckinResult {
					n := running.Add(1)
					defer running.Add(-1)
					for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
					}
					started <- struct{}{}
					select {
					case <-ctx.Done():
					case <-gate:
					}
					status := "claimed"
					if acct.ID == "1" {
						status = "error"
					}
					return CheckinResult{AccountID: acct.ID, Status: status}
				})
			}()
			for i := 0; i < 3; i++ {
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("three workers did not start")
				}
			}
			if cancel {
				stop()
			} else {
				release.Do(func() { close(gate) })
			}
			select {
			case results := <-done:
				want := len(accounts)
				if cancel {
					want = 3
				}
				if len(results) != want || maximum.Load() != 3 || running.Load() != 0 {
					t.Fatalf("results=%d maximum=%d running=%d", len(results), maximum.Load(), running.Load())
				}
				for i, result := range results {
					if result.AccountID != accounts[i].ID {
						t.Fatal("result order changed", results)
					}
				}
				if results[1].Status != "error" {
					t.Fatal("account failure lost", results)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("workers did not exit")
			}
		})
	}
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if got := checkinBatch(ctx, accounts, func(context.Context, *account.Account) CheckinResult {
		t.Error("canceled batch started work")
		return CheckinResult{}
	}); len(got) != 0 {
		t.Fatal("canceled batch produced results", got)
	}
	if err := account.SetGatewayHidden(accounts[1].ID, true); err != nil {
		t.Fatal(err)
	}
	results := checkinBatch(context.Background(), accounts, func(_ context.Context, acct *account.Account) CheckinResult {
		if acct.ID == accounts[1].ID {
			t.Error("hidden account executed")
		}
		return CheckinResult{AccountID: acct.ID}
	})
	if len(results) != len(accounts)-1 || results[1].AccountID != accounts[2].ID {
		t.Fatal("hidden account changed result ordering", results)
	}
}
