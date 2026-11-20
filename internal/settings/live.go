package settings

import "sync"

// Live is something a connector's Run lends to code outside it, such as a
// client or a fetcher: Run sets it and clears it when it ends. Code that acts
// through it does so inside With, which holds it lent for that use: Clear
// waits for every use in progress, so nothing acts through a Run that has
// ended. Get is a look at what is lent now, for a check or a value nothing
// keeps. The zero value holds nothing. Goroutine-safe.
type Live[T any] struct {
	mu   sync.Mutex
	lent *lend[T]
}

// lend is one Set's value and the uses holding it.
type lend[T any] struct {
	value T
	uses  sync.WaitGroup
}

// Set lends value until Clear.
func (l *Live[T]) Set(value T) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lent = &lend[T]{value: value}
}

// Clear takes the value back, and returns once every use of it in With has
// returned.
func (l *Live[T]) Clear() {
	l.mu.Lock()
	lent := l.lent
	l.lent = nil
	l.mu.Unlock()
	if lent != nil {
		lent.uses.Wait()
	}
}

// Get returns the value lent now, and false while there is none. It holds
// nothing: act through the value with With instead.
func (l *Live[T]) Get() (T, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lent == nil {
		var zero T
		return zero, false
	}
	return l.lent.value, true
}

// With calls use with the value lent now and holds it lent until use
// returns, so the Clear that ends its lend waits for use. It returns false
// without calling use while nothing is lent. use must end without waiting
// for the lender's Run to end.
func (l *Live[T]) With(use func(T)) bool {
	l.mu.Lock()
	lent := l.lent
	if lent != nil {
		lent.uses.Add(1)
	}
	l.mu.Unlock()
	if lent == nil {
		return false
	}
	defer lent.uses.Done()
	use(lent.value)
	return true
}
