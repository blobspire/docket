package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/store"
)

type Server struct {
	cfg *Config
	st  *store.Store
	log *log.Logger

	processKick    chan struct{}
	deliverKick    chan struct{}
	transcribeKick chan struct{}
	ideasKick      chan struct{}
	pullKick       map[string]chan struct{}

	finishMu sync.Mutex // one dictation finishes at a time
	// startFn starts a session on a local or pull machine (tests replace it).
	startFn func(ctx context.Context, m Machine, prompt string, args []string) error
	// hold is how long a delivery carrying the human's decision waits before
	// it goes out (undoHold; tests set 0).
	hold time.Duration
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
		processKick: make(chan struct{}, 1), deliverKick: make(chan struct{}, 1), transcribeKick: make(chan struct{}, 1), ideasKick: make(chan struct{}, 1),
		pullKick: map[string]chan struct{}{}, hold: undoHold}
	for _, m := range cfg.Machines {
		s.pullKick[m.Name] = make(chan struct{}, 1)
	}
	for _, k := range cfg.Ignored {
		why := "not a docket setting"
		switch {
		case strings.HasSuffix(k, "physical_keywords"):
			why = "docket no longer guards physical actions; agents name a --place, and the robot's own safety layer decides"
		case strings.HasSuffix(k, "remote_approve"), strings.HasSuffix(k, "brief_at"), strings.HasSuffix(k, "brief_push"),
			strings.HasSuffix(k, "pre_review"), strings.HasSuffix(k, "review_model"):
			why = "no longer used"
		}
		s.log.Printf("config: ignoring %s (%s)", k, why)
	}
	if err := st.ClearStaleStash(); err != nil {
		s.log.Printf("clear stale stash times: %v", err)
	}
	// docket's own health items go here, so it's a project agents can see too.
	st.EnsureProject(cfg.SystemProject, "docket's own alerts: logins, machines, usage limits")
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

func (s *Server) kickProcess()    { kick(s.processKick) }
func (s *Server) kickDeliver()    { kick(s.deliverKick) }
func (s *Server) kickTranscribe() { kick(s.transcribeKick) }
func (s *Server) kickIdeas()      { kick(s.ideasKick) }

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
		if n, err := ingest.ProcessWith(s.st, 200, ingest.Options{LeadProject: s.leadProject, SystemProject: s.cfg.SystemProject}); err != nil {
			s.log.Printf("ingest: %v", err)
		} else if n > 0 {
			s.kickDeliver()
			s.kickIdeas() // a proposal, a session reporting in, a start's result
		}
	})
	loop("deliver", 5*time.Second, s.deliverKick, s.deliverAll)
	loop("chief", 30*time.Second, nil, s.chiefTick)
	loop("leads", 30*time.Second, nil, s.leadTick)
	loop("transcribe", 20*time.Second, s.transcribeKick, s.transcribeTick)
	loop("ideas", 30*time.Second, s.ideasKick, s.ideasTick)

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
