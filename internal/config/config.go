// Package config loads service configuration from XENOS_* environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr     string
	DatabaseURL  string
	Region       string
	PublicURL    string // base URL used in emailed links
	CookieSecure bool   // set Secure on session cookies (disable only for local http)
	TrustProxy   bool   // trust X-Forwarded-For from the reverse proxy (Caddy)

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
	ISpendTenantKey      string
	ISpendWebhookSecret  string
	ISpendMerchantWallet string
	FakeISpendCredit     int64 // dev only: USDT (micro) given to each new customer of the fake iSpend
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:     get("XENOS_HTTP_ADDR", ":8080"),
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

		ISpendURL:            os.Getenv("XENOS_ISPEND_URL"),
		ISpendTenantKey:      os.Getenv("XENOS_ISPEND_TENANT_KEY"),
		ISpendWebhookSecret:  os.Getenv("XENOS_ISPEND_WEBHOOK_SECRET"),
		ISpendMerchantWallet: os.Getenv("XENOS_ISPEND_MERCHANT_WALLET"),
	}
	var err error
	if v := os.Getenv("XENOS_FAKE_ISPEND_CREDIT_UUSDT"); v != "" {
		if c.FakeISpendCredit, err = strconv.ParseInt(v, 10, 64); err != nil {
			return c, fmt.Errorf("XENOS_FAKE_ISPEND_CREDIT_UUSDT: %w", err)
		}
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
	return c, nil
}

func get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Grace is how long suspended VMs are kept after the wallet runs out.
func (Config) Grace() time.Duration { return 72 * time.Hour }
