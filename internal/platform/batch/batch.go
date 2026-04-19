package batch

import "sync"

func Run[T any, R any](items []T, concurrency int, fn func(T) R) []R {
	if concurrency <= 0 {
		concurrency = 1
	}
	results := make([]R, len(items))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(index int, value T) {
			defer wg.Done()
			defer func() { <-sem }()
			results[index] = fn(value)
		}(i, item)
	}
	wg.Wait()
	return results
}
