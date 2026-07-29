package extension

import (
	"sync"
	"testing"

	"github.com/alist-org/alist/v3/internal/driver"
)

// TestGetStorageDriverConcurrentWithEnableDisable is a regression test for a
// data race (EX-T3): GetStorageDriver used to read e.enabled outside of
// r.mu's read lock, racing with Registry.Enable/Disable writing e.enabled
// under the write lock on the same entry. Run with -race to catch a
// regression.
func TestGetStorageDriverConcurrentWithEnableDisable(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterStorageDriver(func() driver.Driver {
		return &fakeMkdirRemove{fakeDriver{name: "RaceDriver"}}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	var readers sync.WaitGroup
	var writers sync.WaitGroup
	stop := make(chan struct{})

	// Readers: hammer GetStorageDriver concurrently.
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = r.GetStorageDriver("RaceDriver")
				}
			}
		}()
	}

	// Writers: toggle Enable/Disable on the same entry concurrently.
	for i := 0; i < 2; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 2000; j++ {
				_ = r.Disable("RaceDriver")
				_ = r.Enable("RaceDriver")
			}
		}()
	}

	// Once the writers are done racing against the readers, stop the
	// readers and wait for them to exit.
	writers.Wait()
	close(stop)
	readers.Wait()
}
