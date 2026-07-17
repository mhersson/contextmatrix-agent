package orchestrator

import (
	"context"
	"time"
)

// StartTicker runs tick every interval until the returned stop func is called
// or ctx is canceled. Stop is idempotent and BLOCKS until the goroutine has
// exited, so no tick can fire after it returns; cancellation also aborts an
// in-flight tick through the tick's context.
func StartTicker(ctx context.Context, interval time.Duration, tick func(context.Context)) func() {
	tickCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-tickCtx.Done():
				return
			case <-ticker.C:
				tick(tickCtx)
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
