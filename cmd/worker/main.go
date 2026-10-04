// Command worker runs background jobs: provisioning, power actions, metering, suspension.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("db", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	q := jobs.New(st.Pool)
	if err := q.RecoverStale(ctx); err != nil {
		log.Error("recover", "err", err)
		os.Exit(1)
	}

	// Handlers are registered here as they are built: "vm.provision", "vm.start",
	// "vm.stop", "vm.reboot", "vm.delete", "vm.suspend", plus the hourly metering ticker.
	handlers := map[string]jobs.Handler{}
	log.Info("worker started")
	q.Run(ctx, handlers, func(err error) { log.Error("job", "err", err) })
}
