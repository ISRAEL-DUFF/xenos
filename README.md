# Xenos

Hourly-billed VPS platform: Go control plane + Proxmox + iSpend wallet billing.
Spec: [VPS V1 Weekend Build Plan.md](<VPS V1 Weekend Build Plan.md>).

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/api` | HTTP server (JSON API, webhooks, embedded dashboard) |
| `cmd/worker` | Background jobs: provisioning, power actions, metering, suspension |
| `internal/config` | `XENOS_*` env config (see `.env.example`) |
| `internal/store` | pgx pool, goose migrations (`migrations/`), sqlc queries (`queries/` → `db/`, run `make sqlc`) |
| `internal/auth` | argon2id passwords, opaque session tokens, rate limiter |
| `internal/mail` | `Mailer` interface (log-only for now) |
| `internal/jobs` | Postgres job queue (`FOR UPDATE SKIP LOCKED`) |
| `internal/proxmox` | `API` interface, REST client (clone, configure, resize, power, status, task wait) and in-memory `Fake` |
| `internal/vm` | Worker-side VM lifecycle: provision, power, delete (the only code that changes VM state) |
| `internal/sshkey` | SSH public key validation |
| `internal/testutil` | Per-test Postgres schemas for integration tests |
| `cmd/xenosctl` | Operator CLI: IP pool, admin grants, VM limits |
| `internal/billing` | `ISpend` interface, in-memory `Fake`, metering helpers |
| `web/` | React + Vite + TS + Tailwind dashboard, embedded via `go:embed` |

## Develop

```sh
make db                      # Postgres 16 via docker compose
cp .env.example .env         # then export the vars
make web                     # build dashboard into web/dist
make run                     # API on :8080, migrations run on start
cd web && npm run dev        # dashboard dev server, proxies /v1 to :8080
make test                    # integration tests skip unless XENOS_TEST_DATABASE_URL is set
go run ./cmd/worker          # separate process: provisions VMs (fake Proxmox when XENOS_PVE_URL is empty)
go run ./cmd/xenosctl ip add 203.0.113.10-203.0.113.14 203.0.113.1   # load the IP pool
```

Integration tests create a throwaway schema per test inside `XENOS_TEST_DATABASE_URL` (the URL must contain `test`) and drop it afterwards; nothing else in that database is touched.

With `XENOS_ISPEND_URL` unset the API uses the in-memory fake iSpend.

## Auth

Opaque random session tokens, stored SHA-256 hashed in Postgres. Two session kinds:

- **cookie** (dashboard): `xenos_session`, httpOnly, SameSite=Lax, Secure unless `XENOS_COOKIE_SECURE=false`. Unsafe methods need an `X-CSRF-Token` header (value from `/v1/auth/me` or the login response).
- **bearer** (CLI/API): send `"token": true` to signup/login, then `Authorization: Bearer <token>`. No CSRF. Each token only works on the transport it was issued for.

Endpoints: `POST /v1/auth/{signup,login,verify,forgot-password,reset-password}`, and when authenticated `GET /v1/auth/me`, `POST /v1/auth/{logout,resend-verification,change-password}`. Password changes and resets revoke all sessions.
Rate limits are in-memory (single instance): signup 3/hour/IP, login 30/15min/IP and 8/15min/account. Behind Caddy set `XENOS_TRUST_PROXY=true`; otherwise `X-Forwarded-For` is ignored.
`requireVerified` and `requireAdmin` middleware exist for the wallet and admin routes to use.

## VMs

`POST /v1/vms {plan, template, hostname?, ssh_key_ids[]}` validates, then in one transaction: locks the user row, checks `vm_limit` and that the wallet covers 24 hours for *all* the user's active VMs plus the new one, inserts the VM, claims a free IPv4 (`FOR UPDATE SKIP LOCKED`) and enqueues `vm.provision`. It answers 202 with the VM in `pending`; poll `GET /v1/vms/{id}`.
Start/stop/reboot/delete validate the current state and enqueue a job. Only the worker (`internal/vm`) changes state: `pending → provisioning → running ⇄ stopped`, `deleting → deleted`, `error`.

Provisioning clones the template, sets cores/memory/cloud-init/IP, resizes the disk, starts the guest and waits for the QEMU agent. Failures retry 3 times; a retry or a restarted worker first removes any half-built guest under the same VMID. After the last attempt the guest is destroyed, the IP released and the VM marked `error` (the IP stays reserved if the guest could not be removed).

Run exactly one worker process (startup requeues every job left `running`). For a test host without KVM set `XENOS_PVE_DISABLE_KVM=true` and a long `XENOS_PROVISION_TIMEOUT`.

## Status

Phase 2 is code-complete and tested against a fake Proxmox; it is **not yet verified on a real Proxmox host** (its "done when" needs one). Implemented: schema, config, job queue, Proxmox client, fake iSpend, auth, SSH keys, VM create/list/get/power/delete with the provisioning worker, `xenosctl`.
Still 501: wallet, `POST /v1/webhooks/ispend` (Phase 3). Not built: metering and suspension (Phase 3), admin endpoints, the VM dashboard pages (Phase 5).
Plan prices in the seed migration are placeholders. Emails are logged, not sent, until a provider is chosen.
