package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func orderedJobs(n int) []transformJob {
	jobs := make([]transformJob, n)
	for i := range jobs {
		jobs[i].Index = i
	}
	return jobs
}

func awaitPipeline(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("ordered pipeline failed to join workers")
		return nil
	}
}

func TestTransformOrderedBoundsAndOrder(t *testing.T) {
	for _, workers := range []int{1, 2, 8} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			const count = 73
			var mu sync.Mutex
			consumed, transformed := 0, 0
			var got []int
			later := make(chan struct{}, 2*workers)
			done := make(chan error, 1)
			go func() {
				done <- transformOrdered(context.Background(), orderedJobs(count), workers, func(job transformJob) transformResult {
					mu.Lock()
					transformed++
					overrun := job.Index >= consumed+2*workers
					mu.Unlock()
					if overrun {
						return transformResult{Err: fmt.Errorf("page %d admitted beyond unconsumed window", job.Index)}
					}
					if workers > 1 && job.Index == 0 {
						for range 2*workers - 1 {
							<-later
						}
					} else if job.Index < 2*workers {
						later <- struct{}{}
					}
					return transformResult{Record: record{SourceBytes: job.Index}}
				}, func(rec record) error {
					mu.Lock()
					defer mu.Unlock()
					got = append(got, rec.SourceBytes)
					consumed++
					return nil
				})
			}()
			if err := awaitPipeline(t, done); err != nil {
				t.Fatal(err)
			}
			want := make([]int, count)
			for i := range want {
				want[i] = i
			}
			if transformed != count || !reflect.DeepEqual(got, want) {
				t.Fatalf("transformed %d pages, consumed order %v", transformed, got)
			}
		})
	}
}

func TestTransformOrderedFailureJoinsWorkers(t *testing.T) {
	for _, failure := range []string{"transform", "consume", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New(failure)
			var active atomic.Int32
			var calls atomic.Int32
			consumed := 0
			done := make(chan error, 1)
			go func() {
				done <- transformOrdered(ctx, orderedJobs(1000), 4, func(job transformJob) transformResult {
					active.Add(1)
					defer active.Add(-1)
					calls.Add(1)
					if job.Index == 0 {
						if failure == "transform" {
							return transformResult{Err: want}
						}
						if failure == "cancel" {
							cancel()
						}
					}
					return transformResult{}
				}, func(record) error {
					consumed++
					return want
				})
			}()
			if failure == "cancel" {
				want = context.Canceled
			}
			if err := awaitPipeline(t, done); !errors.Is(err, want) {
				t.Fatalf("expected %v, got %v", want, err)
			}
			if active.Load() != 0 || calls.Load() == 0 || calls.Load() > 8 {
				t.Fatalf("unjoined workers or unbounded admission: active=%d calls=%d", active.Load(), calls.Load())
			}
			if failure == "consume" && consumed != 1 || failure != "consume" && consumed != 0 {
				t.Fatalf("unexpected writes after %s: %d", failure, consumed)
			}
		})
	}
}

func TestTransformOrderedEmptyAndSmall(t *testing.T) {
	for _, count := range []int{0, 1, 3} {
		for _, workers := range []int{0, 32} {
			consumed := 0
			err := transformOrdered(context.Background(), orderedJobs(count), workers, func(transformJob) transformResult {
				return transformResult{}
			}, func(record) error { consumed++; return nil })
			if err != nil || consumed != count {
				t.Fatalf("count=%d workers=%d: consumed=%d error=%v", count, workers, consumed, err)
			}
		}
	}
}
