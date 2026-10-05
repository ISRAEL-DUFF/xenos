# Browser journey test

Drives the real dashboard in Chromium: sign up, verify email, add a key, create a VM and watch it provision, stop/start, delete, top-up pages, the admin area, and the phone layout (no sideways scrolling). It also fails on any console error, which catches Content-Security-Policy violations.

It needs a stack with fake external services and an embedded worker:

```sh
make web
export XENOS_DATABASE_URL=postgres://…  XENOS_HTTP_ADDR=:8080 XENOS_COOKIE_SECURE=false
export XENOS_RUN_WORKER=true XENOS_FAKE_ISPEND_CREDIT_UUSDT=5000000   # iSpend and Proxmox are the in-memory fakes
export XENOS_METER_SPREAD_MINUTES=0   # charge each hour straight away so the test need not wait up to 40 minutes
go run ./cmd/api > /tmp/xenos-api.log 2>&1 &
go run ./cmd/xenosctl ip add 203.0.113.10-203.0.113.20 203.0.113.1

cd web && npm install
E2E_API_LOG=/tmp/xenos-api.log E2E_SHOTS=/tmp/shots \
E2E_PROMOTE='cd .. && go run ./cmd/xenosctl admin grant "$E2E_EMAIL"' npm run e2e
```

Set `E2E_CHROMIUM` to a Chromium/Chrome binary if the default (`/opt/pw-browsers/chromium-1194/chrome-linux/chrome`) does not exist. Omit `E2E_PROMOTE` to skip the admin section.
