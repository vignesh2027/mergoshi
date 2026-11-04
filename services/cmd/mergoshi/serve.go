package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"mergoshi/internal/web"
)

// serve runs the dashboard, and the attesters too when asked.
func (a *app) serve(ctx context.Context, addr, network string, withAttesters bool, extra func(*http.ServeMux) []web.NavLink) error {
	tc, err := a.client()
	if err != nil {
		return err
	}
	s := &web.Server{Token: tc, Network: network, SimDir: a.env.Or("MERGOSHI_SIM_DIR", "sim/out")}
	h, err := s.Handler()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/", h)
	if extra != nil {
		s.Extra = extra(mux)
	}
	if withAttesters {
		atts, err := a.attesters(nil, []int{1, 2, 3})
		if err != nil {
			return err
		}
		for _, at := range atts {
			go func() { _ = at.Run(ctx, 20*time.Second) }()
		}
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Println("dashboard on http://localhost" + addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
