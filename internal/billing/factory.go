package billing

import (
	"log/slog"

	"github.com/israel-duff/xenos/internal/config"
)

// FromConfig returns the real iswallet client when XENOS_ISPEND_URL is set, otherwise the
// in-memory fake (its state is per process, so use XENOS_RUN_WORKER=true in development).
func FromConfig(cfg config.Config, log *slog.Logger) (ISpend, error) {
	if cfg.ISpendURL == "" {
		log.Warn("XENOS_ISPEND_URL unset: using in-memory fake iSpend")
		f := NewFake(150_000) // ₦1,500/USDT, in kobo
		f.SignupCredit = cfg.FakeISpendCredit
		return f, nil
	}
	return NewISWallet(ISWalletConfig{BaseURL: cfg.ISpendURL, APIKey: cfg.ISpendAPIKey, MerchantWallet: cfg.ISpendMerchantWallet,
		OwnerPrefix: cfg.ISpendOwnerPrefix, USDTDecimals: cfg.ISpendUSDTDecimals, Log: log})
}
