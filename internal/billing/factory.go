package billing

import (
	"errors"
	"log/slog"

	"github.com/israel-duff/xenos/internal/config"
)

// FromConfig returns the real iSpend client when configured, otherwise the
// in-memory fake (state is per process, so use XENOS_RUN_WORKER=true in dev).
func FromConfig(cfg config.Config, log *slog.Logger) (ISpend, error) {
	if cfg.ISpendURL == "" {
		log.Warn("XENOS_ISPEND_URL unset: using in-memory fake iSpend")
		f := NewFake(150_000) // ₦1,500/USDT, in kobo
		f.SignupCredit = cfg.FakeISpendCredit
		return f, nil
	}
	return nil, errors.New("real iSpend client not implemented yet")
}
