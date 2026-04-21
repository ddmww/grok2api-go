package proxy

import (
	"sync"
	"time"

	"github.com/ddmww/grok2api-go/internal/platform/config"
)

type ClearanceScheduler struct {
	cfg    *config.Service
	runtime *Runtime
	wg     sync.WaitGroup
	stopCh chan struct{}
}

func NewClearanceScheduler(cfg *config.Service, runtime *Runtime) *ClearanceScheduler {
	return &ClearanceScheduler{
		cfg:     cfg,
		runtime: runtime,
		stopCh:  make(chan struct{}),
	}
}

func (s *ClearanceScheduler) Start() {
	if s == nil || s.cfg == nil || s.runtime == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if s.enabled() {
			s.runtime.WarmUp()
		}
		for {
			if s.enabled() {
				s.runtime.RefreshClearanceSafe()
			}
			interval := s.cfg.GetInt("proxy.clearance.refresh_interval", 600)
			if interval <= 0 {
				interval = 600
			}
			timer := time.NewTimer(time.Duration(interval) * time.Second)
			select {
			case <-timer.C:
				s.runtime.ResetAll()
			case <-s.stopCh:
				if !timer.Stop() {
					<-timer.C
				}
				return
			}
		}
	}()
}

func (s *ClearanceScheduler) enabled() bool {
	mode := s.cfg.GetString("proxy.clearance.mode", "none")
	switch mode {
	case "manual", "flaresolverr":
		return true
	default:
		return false
	}
}

func (s *ClearanceScheduler) Stop() {
	if s == nil {
		return
	}
	close(s.stopCh)
	s.wg.Wait()
}
