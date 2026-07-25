package agent

import "sync"

// ChildTurnPermit owns one unit of child-turn concurrency until released.
type ChildTurnPermit interface {
	Release()
}

type childTurnSemaphore chan struct{}

func newChildTurnSemaphore(limit int) childTurnSemaphore {
	return make(childTurnSemaphore, limit)
}

func (s childTurnSemaphore) tryAcquire() (ChildTurnPermit, bool) {
	select {
	case s <- struct{}{}:
		return &childTurnPermit{release: func() { <-s }}, true
	default:
		return nil, false
	}
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
