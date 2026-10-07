# Prometheus for Xenos

The API and the worker each serve Prometheus metrics on a **loopback-only** listener. Nothing is added to the public port and Caddy does not proxy it.

| Process | Setting | Default |
|---|---|---|
| API (`xenos`) | `XENOS_METRICS_ADDR` | `127.0.0.1:9090` |
| Worker (`xenos-worker`) | `XENOS_WORKER_METRICS_ADDR` | `127.0.0.1:9091` |

Set either to `off` to disable it. When the API runs the worker inside itself (`XENOS_RUN_WORKER=true`, development only) there is one listener and one registry.

## What is exported

* **API:** requests by route pattern (never the raw path), method and status class; rate-limit refusals by limiter; webhooks by outcome; open consoles; wallet service calls by method and result.
* **Worker:** jobs by kind and result with durations; the metering pass (duration, last success, wallet service down); host pool and RAM commitment; and, read from Postgres at scrape time (cached 15 s): VMs by state, queued/running/failed jobs by kind, open (pending, unpaid) charges and their amount, the oldest pending charge's age, unpaid total, conversions stuck, free and total IPv4, heartbeat age, bad-signature webhooks in the last hour.
* Go runtime and process metrics on both.

## Setting it up on the control-plane VM

```sh
apt install prometheus
cp prometheus.yml alerts.yml /etc/prometheus/
promtool check config /etc/prometheus/prometheus.yml
systemctl restart prometheus
curl -s 127.0.0.1:9090/metrics | head   # the API
curl -s 127.0.0.1:9091/metrics | head   # the worker
```

Prometheus itself listens on `127.0.0.1:9090` by default, which collides with `XENOS_METRICS_ADDR`. Either start Prometheus with `--web.listen-address=127.0.0.1:9095` (and adjust any dashboard URL), or set `XENOS_METRICS_ADDR` to another port and change the scrape target.

The alert rules in `alerts.yml` complement the chat alerts the worker already sends: they cover what only a time series can show, and they still fire when the worker is the thing that is down. Add Alertmanager (or point Grafana alerting at the same expressions) to receive them.
