package agent

import (
	"context"
	"sync"
)

// ChildTurnPermit owns one unit of child-turn concurrency until released.
type ChildTurnPermit interface {
	Release()
}

type childTurnSemaphore struct {
	mu      sync.Mutex
	used    int
	limit   int
	waiters []chan struct{}
}

func newChildTurnSemaphore(limit int) *childTurnSemaphore {
	return &childTurnSemaphore{limit: limit}
}

func (s *childTurnSemaphore) tryAcquire() (ChildTurnPermit, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used == s.limit || len(s.waiters) != 0 {
		return nil, false
	}
	s.used++
	return &childTurnPermit{release: s.release}, true
}

func (s *childTurnSemaphore) acquire(ctx context.Context) (ChildTurnPermit, error) {
	if s == nil {
		return nil, ErrChildConcurrency
	}
	s.mu.Lock()
	if s.used < s.limit && len(s.waiters) == 0 {
		s.used++
		s.mu.Unlock()
		return &childTurnPermit{release: s.release}, nil
	}
	ready := make(chan struct{})
	s.waiters = append(s.waiters, ready)
	s.mu.Unlock()
	select {
	case <-ready:
		return &childTurnPermit{release: s.release}, nil
	case <-ctx.Done():
		s.mu.Lock()
		for index, waiter := range s.waiters {
			if waiter == ready {
				s.waiters = append(s.waiters[:index], s.waiters[index+1:]...)
				s.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		s.mu.Unlock()
		<-ready
		s.release()
		return nil, ctx.Err()
	}
}

func (s *childTurnSemaphore) release() {
	s.mu.Lock()
	if len(s.waiters) == 0 {
		s.used--
		s.mu.Unlock()
		return
	}
	ready := s.waiters[0]
	s.waiters = s.waiters[1:]
	close(ready)
	s.mu.Unlock()
}

type childTurnPermit struct {
	once    sync.Once
	release func()
}

func (p *childTurnPermit) Release() {
	if p != nil {
		p.once.Do(p.release)
	}
}
