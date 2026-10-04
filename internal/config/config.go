// Package config loads service configuration from XENOS_* environment variables.
package config

import (
	"fmt"
	"os"
)

type Config struct {
	HTTPAddr    string
	DatabaseURL string
	Region      string

	PVEURL         string
	PVENode        string
	PVETokenID     string
	PVETokenSecret string
	PVEStorage     string
	PVEBridge      string

	ISpendURL            string
	ISpendTenantKey      string
	ISpendWebhookSecret  string
	ISpendMerchantWallet string
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:    get("XENOS_HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("XENOS_DATABASE_URL"),
		Region:      get("XENOS_REGION", "eu-de-1"),

		PVEURL:         os.Getenv("XENOS_PVE_URL"),
		PVENode:        get("XENOS_PVE_NODE", "pve"),
		PVETokenID:     os.Getenv("XENOS_PVE_TOKEN_ID"),
		PVETokenSecret: os.Getenv("XENOS_PVE_TOKEN_SECRET"),
		PVEStorage:     get("XENOS_PVE_STORAGE", "vmdata"),
		PVEBridge:      get("XENOS_PVE_BRIDGE", "vmbr0"),

		ISpendURL:            os.Getenv("XENOS_ISPEND_URL"),
		ISpendTenantKey:      os.Getenv("XENOS_ISPEND_TENANT_KEY"),
		ISpendWebhookSecret:  os.Getenv("XENOS_ISPEND_WEBHOOK_SECRET"),
		ISpendMerchantWallet: os.Getenv("XENOS_ISPEND_MERCHANT_WALLET"),
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
