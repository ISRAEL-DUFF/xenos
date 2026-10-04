# Xenos

Hourly-billed VPS platform: Go control plane + Proxmox + iSpend wallet billing.
Spec: [VPS V1 Weekend Build Plan.md](<VPS V1 Weekend Build Plan.md>).

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/api` | HTTP server (JSON API, webhooks, embedded dashboard) |
| `cmd/worker` | Background jobs: provisioning, power actions, metering, suspension |
| `internal/config` | `XENOS_*` env config (see `.env.example`) |
| `internal/store` | pgx pool + goose migrations (`migrations/`) |
| `internal/jobs` | Postgres job queue (`FOR UPDATE SKIP LOCKED`) |
| `internal/proxmox` | Proxmox REST client (clone, configure, resize, power, task wait) |
| `internal/billing` | `ISpend` interface, in-memory `Fake`, metering helpers |
| `web/` | React + Vite + TS + Tailwind dashboard, embedded via `go:embed` |

## Develop

```sh
make db                      # Postgres 16 via docker compose
cp .env.example .env         # then export the vars
make web                     # build dashboard into web/dist
make run                     # API on :8080, migrations run on start
cd web && npm run dev        # dashboard dev server, proxies /v1 to :8080
make test
```

With `XENOS_ISPEND_URL` unset the API uses the in-memory fake iSpend.

## Status

Scaffold only. Implemented: schema, config, job queue, Proxmox client, fake iSpend, `GET /v1/plans`, `GET /v1/templates`, dashboard shell.
Everything else under `/v1` returns 501 until built (auth, SSH keys, VMs, wallet, webhooks, worker handlers, metering, admin).
Plan prices in the seed migration are placeholders.
