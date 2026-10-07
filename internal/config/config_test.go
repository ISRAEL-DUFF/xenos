package config

import (
	"strings"
	"testing"
)

func TestProductionRefusesDevelopmentDefaults(t *testing.T) {
	err := Config{Env: "production", CookieSecure: false}.checkProduction()
	if err == nil {
		t.Fatal("an empty production config must be refused")
	}
	for _, want := range []string{"XENOS_ISPEND_URL", "XENOS_SMTP_HOST", "XENOS_COOKIE_SECURE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}
	ok := Config{Env: "production", CookieSecure: true, ISpendURL: "https://x", ISpendAPIKey: "k", SMTPHost: "smtp", MailFrom: "a@b.c"}
	if err := ok.checkProduction(); err != nil {
		t.Errorf("a complete production config: %v", err)
	}
	if err := (Config{}).checkProduction(); err != nil {
		t.Errorf("development must stay unrestricted: %v", err)
	}
}
