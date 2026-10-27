package main

import (
	"context"
	"sync"
)

// transformOrdered consumes in input order with at most two pages per worker in flight.
// Errors or cancellation stop admission and join all workers before returning. A transform
// already running must finish; the caller owns cancellation of its underlying I/O.
func transformOrdered(ctx context.Context, jobs []transformJob, workers int, transform func(transformJob) transformResult, consume func(record) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	workers = min(max(workers, 1), len(jobs))
	window := min(2*workers, len(jobs))
	ctx, cancel := context.WithCancel(ctx)
	jobCh := make(chan int, window)
	results := make([]chan transformResult, window)
	for i := range results {
		results[i] = make(chan transformResult, 1)
	}
	var wg sync.WaitGroup
	defer func() {
		cancel()
		close(jobCh)
		wg.Wait()
	}()
	for range workers {
		wg.Go(func() {
			for index := range jobCh {
				if ctx.Err() != nil {
					return
				}
				result := transform(jobs[index])
				select {
				case results[index%window] <- result:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	for i := range window {
		jobCh <- i
	}
	for i := range jobs {
		var result transformResult
		select {
		case result = <-results[i%window]:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if result.Err != nil {
			return result.Err
		}
		if err := consume(result.Record); err != nil {
			return err
		}
		// INVARIANT: A ring slot is reused only after its previous page has been consumed.
		result = transformResult{}
		if next := i + window; next < len(jobs) {
			jobCh <- next
		}
	}
	return ctx.Err()
}
