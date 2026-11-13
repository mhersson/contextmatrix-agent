package cli

import (
	"log/slog"
	"sync"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/filelog"
)

// startLogPruner removes card logs untouched for retention, once before it
// returns and then every interval. The returned stop is idempotent and waits
// for the sweep loop to exit.
func startLogPruner(files *filelog.Logger, retention, interval time.Duration, logger *slog.Logger) func() {
	prune := func() {
		if n := files.Prune(time.Now(), retention); n > 0 {
			logger.Info("pruned card logs", "removed", n, "retention", retention)
		}
	}

	prune()

	done := make(chan struct{})
	exited := make(chan struct{})

	go func() {
		defer close(exited)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				prune()
			case <-done:
				return
			}
		}
	}()

	var once sync.Once

	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}
