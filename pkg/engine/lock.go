package engine

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// DefaultLockTimeout is how long a process waits for another process to
// finish with the same .bsl file before Open or Init returns ErrLocked.
const DefaultLockTimeout = 30 * time.Second

// lockPath takes an exclusive advisory lock on a sidecar file that outlives
// checkpoint renames of the .bsl itself. The returned function releases it.
func lockPath(path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
