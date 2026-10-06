// Package config loads service configuration from XENOS_* environment variables.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env          string // "production" turns on startup checks that refuse unsafe defaults
	HTTPAddr     string
	DatabaseURL  string
	Region       string
	PublicURL    string // base URL used in emailed links
	CookieSecure bool   // set Secure on session cookies (disable only for local http)
	TrustProxy   bool   // trust X-Forwarded-For from the reverse proxy (Caddy)
	// TrustedProxies are extra proxy addresses (besides loopback) whose X-Forwarded-For is believed.
	TrustedProxies                                   []netip.Prefix
	SMTPHost, SMTPPort, SMTPUser, SMTPPass, MailFrom string // transactional email; any SMTP provider

	PVEURL         string
	PVENode        string
	PVETokenID     string
	PVETokenSecret string
	PVEStorage     string
	PVEBridge      string
	PVEDisk        string // boot disk name on cloned templates
	PVEInsecureTLS bool   // accept the host's self-signed certificate
	PVEDisableKVM  bool   // test hosts without hardware virtualisation (very slow)

	ProvisionTimeout  time.Duration // how long to wait for a new VM's guest agent
	WorkerConcurrency int
	RunWorker         bool   // run the worker inside the API process (development)
	IPv4PrefixLen     int    // prefix length written into each VM's network config
	IPv6Prefix        string // /64 to allocate VM addresses from; empty disables IPv6
	IPv6Gateway       string
	Nameservers       string // space-separated

	ISpendURL            string
	ISpendAPIKey         string
	ISpendOwnerPrefix    string // namespaces owner_ref at iswallet (global across tenants)
	ISpendUSDTDecimals   int    // scale of iswallet's USDT minor unit (6); a guard against balance responses that disagree
	ISpendWebhookSecret  string
	ISpendMerchantWallet string
	TelegramBotToken     string // operator alerts; both Telegram values or neither
	TelegramChatID       string
	AlertEmail           string // operator alerts by email in addition to / instead of Telegram
	DepositLimitKobo     int64  // shown to customers: the most a basic (TIER_1) account may receive per transfer and per day; 0 hides it
	MeterSpreadMinutes   int    // spread hourly charges over this many minutes after the hour, to stay under iswallet's rate limit
	FakeISpendCredit     int64  // dev only: USDT (micro) given to each new customer of the fake iSpend
}

func Load() (Config, error) {
	c := Config{
		Env:          get("XENOS_ENV", "development"),
		HTTPAddr:     get("XENOS_HTTP_ADDR", "127.0.0.1:8080"),
		SMTPHost:     os.Getenv("XENOS_SMTP_HOST"),
		SMTPPort:     get("XENOS_SMTP_PORT", "587"),
		SMTPUser:     os.Getenv("XENOS_SMTP_USER"),
		SMTPPass:     os.Getenv("XENOS_SMTP_PASS"),
		MailFrom:     os.Getenv("XENOS_MAIL_FROM"),
		DatabaseURL:  os.Getenv("XENOS_DATABASE_URL"),
		Region:       get("XENOS_REGION", "eu-de-1"),
		PublicURL:    get("XENOS_PUBLIC_URL", "http://localhost:8080"),
		CookieSecure: get("XENOS_COOKIE_SECURE", "true") != "false",
		TrustProxy:   get("XENOS_TRUST_PROXY", "false") == "true",

		PVEURL:         os.Getenv("XENOS_PVE_URL"),
		PVENode:        get("XENOS_PVE_NODE", "pve"),
		PVETokenID:     os.Getenv("XENOS_PVE_TOKEN_ID"),
		PVETokenSecret: os.Getenv("XENOS_PVE_TOKEN_SECRET"),
		PVEStorage:     get("XENOS_PVE_STORAGE", "vmdata"),
		PVEBridge:      get("XENOS_PVE_BRIDGE", "vmbr0"),
		PVEDisk:        get("XENOS_PVE_DISK", "scsi0"),
		PVEInsecureTLS: get("XENOS_PVE_INSECURE_TLS", "false") == "true",
		PVEDisableKVM:  get("XENOS_PVE_DISABLE_KVM", "false") == "true",

		IPv6Prefix:  os.Getenv("XENOS_IPV6_PREFIX"),
		IPv6Gateway: os.Getenv("XENOS_IPV6_GATEWAY"),
		Nameservers: get("XENOS_NAMESERVERS", "1.1.1.1 9.9.9.9"),
		RunWorker:   get("XENOS_RUN_WORKER", "false") == "true",

		TelegramBotToken: os.Getenv("XENOS_TELEGRAM_BOT_TOKEN"),
		TelegramChatID:   os.Getenv("XENOS_TELEGRAM_CHAT_ID"),
		AlertEmail:       os.Getenv("XENOS_ALERT_EMAIL"),

		ISpendURL:            os.Getenv("XENOS_ISPEND_URL"),
		ISpendAPIKey:         os.Getenv("XENOS_ISPEND_API_KEY"),
		ISpendOwnerPrefix:    get("XENOS_ISPEND_OWNER_PREFIX", "xenos"),
		ISpendWebhookSecret:  os.Getenv("XENOS_ISPEND_WEBHOOK_SECRET"),
		ISpendMerchantWallet: os.Getenv("XENOS_ISPEND_MERCHANT_WALLET"),
	}
	var err error
	if v := os.Getenv("XENOS_FAKE_ISPEND_CREDIT_UUSDT"); v != "" {
		if c.FakeISpendCredit, err = strconv.ParseInt(v, 10, 64); err != nil {
			return c, fmt.Errorf("XENOS_FAKE_ISPEND_CREDIT_UUSDT: %w", err)
		}
	}
	if c.ISpendUSDTDecimals, err = strconv.Atoi(get("XENOS_ISPEND_USDT_DECIMALS", "6")); err != nil {
		return c, fmt.Errorf("XENOS_ISPEND_USDT_DECIMALS: %w", err)
	}
	if c.DepositLimitKobo, err = strconv.ParseInt(get("XENOS_DEPOSIT_LIMIT_KOBO", "5000000"), 10, 64); err != nil {
		return c, fmt.Errorf("XENOS_DEPOSIT_LIMIT_KOBO: %w", err)
	}
	if c.MeterSpreadMinutes, err = strconv.Atoi(get("XENOS_METER_SPREAD_MINUTES", "40")); err != nil || c.MeterSpreadMinutes < 0 || c.MeterSpreadMinutes > 55 {
		return c, fmt.Errorf("XENOS_METER_SPREAD_MINUTES must be 0-55")
	}
	if c.ProvisionTimeout, err = time.ParseDuration(get("XENOS_PROVISION_TIMEOUT", "3m")); err != nil {
		return c, fmt.Errorf("XENOS_PROVISION_TIMEOUT: %w", err)
	}
	if c.WorkerConcurrency, err = strconv.Atoi(get("XENOS_WORKER_CONCURRENCY", "4")); err != nil || c.WorkerConcurrency < 1 {
		return c, fmt.Errorf("XENOS_WORKER_CONCURRENCY must be a positive integer")
	}
	if c.IPv4PrefixLen, err = strconv.Atoi(get("XENOS_IPV4_PREFIX_LEN", "32")); err != nil || c.IPv4PrefixLen < 1 || c.IPv4PrefixLen > 32 {
		return c, fmt.Errorf("XENOS_IPV4_PREFIX_LEN must be 1-32")
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("XENOS_DATABASE_URL is required")
	}
	for _, f := range strings.Fields(strings.ReplaceAll(os.Getenv("XENOS_TRUSTED_PROXIES"), ",", " ")) {
		p, err := netip.ParsePrefix(f)
		if err != nil {
			a, aerr := netip.ParseAddr(f)
			if aerr != nil {
				return c, fmt.Errorf("XENOS_TRUSTED_PROXIES: %q is not an address or CIDR", f)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c.TrustedProxies = append(c.TrustedProxies, p)
	}
	if err := c.checkProduction(); err != nil {
		return c, err
	}
	return c, nil
}

// checkProduction refuses configurations that are only safe for development: silently using the
// in-memory fake iSpend (which can hand out free credit), emailing nobody (reset and verification
// would not work), insecure cookies, or a database link without TLS.
func (c Config) checkProduction() error {
	if c.Env != "production" {
		return nil
	}
	var bad []string
	if c.ISpendURL == "" || c.ISpendAPIKey == "" {
		bad = append(bad, "XENOS_ISPEND_URL and XENOS_ISPEND_API_KEY are required (the fake iSpend is development only)")
	}
	if c.SMTPHost == "" || c.MailFrom == "" {
		bad = append(bad, "XENOS_SMTP_HOST and XENOS_MAIL_FROM are required (email verification and password reset need real mail)")
	}
	if !c.CookieSecure {
		bad = append(bad, "XENOS_COOKIE_SECURE must not be false")
	}
	if len(bad) > 0 {
		return fmt.Errorf("XENOS_ENV=production: %s", strings.Join(bad, "; "))
	}
	return nil
}

func get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Grace is how long suspended VMs are kept after the wallet runs out.
func (Config) Grace() time.Duration { return 72 * time.Hour }
