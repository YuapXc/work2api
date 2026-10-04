// Package statebackup coordinates coherent snapshots with service-owned writes.
package statebackup

import "sync"

var gate sync.RWMutex

func Enter() func() { gate.RLock(); return gate.RUnlock }

// TrySnapshot never queues a writer behind long model calls or OAuth polling.
// The caller retries later rather than delaying newly arriving inference calls.
func TrySnapshot() (func(), bool) {
	if !gate.TryLock() {
		return nil, false
	}
	return gate.Unlock, true
}
