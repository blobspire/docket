package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/store"
)

type Server struct {
	cfg *Config
	st  *store.Store
	log *log.Logger

	processKick chan struct{}
	deliverKick chan struct{}
	pullKick    map[string]chan struct{}
}

// Open loads the store for cfg. Commands that only touch the database (for
// example `docket server receive`) use this without running the loops.
func Open(cfg *Config) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "docket.db"))
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, st: st, log: log.New(os.Stderr, "docket: ", log.LstdFlags),
		processKick: make(chan struct{}, 1), deliverKick: make(chan struct{}, 1), pullKick: map[string]chan struct{}{}}
	for _, m := range cfg.Machines {
		s.pullKick[m.Name] = make(chan struct{}, 1)
	}
	return s, nil
}

func (s *Server) Close() error { return s.st.Close() }

// Store exposes the database (for commands such as import).
func (s *Server) Store() *store.Store { return s.st }

func kick(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (s *Server) kickProcess() { kick(s.processKick) }
func (s *Server) kickDeliver() { kick(s.deliverKick) }

// KickPull asks for an immediate collection from a machine (or all).
func (s *Server) KickPull(name string) {
	for n, c := range s.pullKick {
		if name == "" || n == name {
			kick(c)
		}
	}
}

// Run starts every loop and the web server, until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	loop := func(name string, every time.Duration, kickc chan struct{}, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				func() {
					defer func() {
						if r := recover(); r != nil {
							s.log.Printf("%s: panic: %v", name, r)
						}
					}()
					fn(ctx)
				}()
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				case <-kickc:
				}
			}
		}()
	}

	for _, m := range s.cfg.Machines {
		if m.Mode == "push" {
			continue
		}
		m := m
		loop("pull "+m.Name, m.Interval.Duration, s.pullKick[m.Name], func(ctx context.Context) {
			if err := s.pull(ctx, m); err != nil {
				s.log.Printf("%s: %v", m.Name, err)
			}
		})
	}
	loop("ingest", 2*time.Second, s.processKick, func(context.Context) {
		if n, err := ingest.Process(s.st, 200); err != nil {
			s.log.Printf("ingest: %v", err)
		} else if n > 0 {
			s.kickDeliver()
		}
	})
	loop("deliver", 5*time.Second, s.deliverKick, s.deliverAll)
	loop("chief", 30*time.Second, nil, s.chiefTick)
	loop("leads", 30*time.Second, nil, s.leadTick)

	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.log.Printf("serving on http://%s", s.cfg.Listen)
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shut)
	wg.Wait()
	return nil
}
