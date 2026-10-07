package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/httpapi"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/metrics"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/worker"
	"github.com/israel-duff/xenos/web"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	ispend, err := billing.FromConfig(cfg, log)
	if err != nil {
		return err
	}
	if cfg.Env == "production" && !cfg.TrustProxy {
		log.Warn("XENOS_TRUST_PROXY is false: behind a reverse proxy every client shares one IP, so per-IP rate limits apply to all users together")
	}
	mailer, err := mail.New(cfg, log)
	if err != nil {
		return err
	}

	m := metrics.New()
	ispend = metrics.WrapISpend(ispend, m)
	srv := httpapi.NewServer(cfg, st, jobs.New(st.Pool), ispend, mailer, log, web.Dist())
	hostSet, err := worker.NewHosts(ctx, cfg, st, log)
	if err != nil {
		return err
	}
	if cfg.PVEURL != "" || cfg.HostsFile != "" || cfg.RunWorker { // capacity view and console need a host (the fake, in development)
		srv.PVE, srv.Hosts = hostSet, hostSet
	}
	srv.Metrics = m
	hs := &http.Server{Addr: cfg.HTTPAddr, Handler: srv.Router(), ReadHeaderTimeout: 10 * time.Second}

	if cfg.RunWorker {
		// Development convenience: the fake iSpend only lives inside one process.
		log.Warn("XENOS_RUN_WORKER=true: running the worker inside the API process")
		go func() {
			if err := worker.RunWith(ctx, cfg, st, ispend, mailer, log, hostSet, m); err != nil {
				log.Error("worker stopped", "err", err)
			}
		}()
	}

	go func() {
		if err := metrics.Serve(ctx, cfg.MetricsAddr, m, log); err != nil && ctx.Err() == nil {
			log.Error("metrics listener", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sc)
	}()
	log.Info("listening", "addr", cfg.HTTPAddr)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
