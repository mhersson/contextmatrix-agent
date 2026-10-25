# Observability

`serve` exposes Prometheus metrics on a **separate admin listener** - metrics
never ride the public webhook port. `admin_port: 0` (the default) disables the
listener; a typical admin port is `9093`, and it must differ from `port`
(default `9092`). Env override: `CMX_ADMIN_PORT`.

The listener binds `admin_bind_addr`, default `127.0.0.1` (loopback-only). Set
a LAN address, or `0.0.0.0`, to let an external Prometheus scrape it; `serve`
then logs a warning at startup, and a firewall should restrict access. Env
override: `CMX_ADMIN_BIND_ADDR`.

`GET /metrics` accepts either credential:

- The signed-GET HMAC the webhook routes use: sign `METHOD\nURI\nTS.BODY` with
  the backend `api_key`, and send the result as `X-Signature-256: sha256=<hex>`
  with the timestamp in `X-Webhook-Timestamp`.
- When `metrics_token` is set, `Authorization: Bearer <metrics_token>`, for
  scrapers that cannot sign requests (the `authorization.credentials` value in
  a Prometheus scrape config). A wrong token falls through to the HMAC check,
  so every failure is the same 401. Env override: `CMX_METRICS_TOKEN`.

An empty `metrics_token` (the default) keeps `/metrics` HMAC-only. The startup
log line `admin endpoints registered` reports which applies in its
`metrics_auth` field: `hmac` or `hmac+bearer`.

Metrics live on a dedicated registry (`internal/metrics`, alongside the
standard `go_*`/`process_*` collectors). Endpoint labels are bounded by an
allowlist (`NormalizeEndpoint`); unknown paths collapse to `other`. No
`card_id`/`project` labels anywhere.

| Metric                                      | Type      | Labels                                                            |
| ------------------------------------------- | --------- | ----------------------------------------------------------------- |
| `cm_agent_webhook_requests_total`           | counter   | `endpoint`, `status`, `code`                                      |
| `cm_agent_webhook_request_duration_seconds` | histogram | `endpoint`                                                        |
| `cm_agent_container_duration_seconds`       | histogram | `outcome` (`success`/`failure`/`timeout`/`killed`/`idle_timeout`) |
| `cm_agent_running_containers`               | gauge     | -                                                                 |
| `cm_agent_callback_retries_total`           | counter   | `endpoint` (`status`/`verify-autonomous`)                         |
| `cm_agent_broadcaster_drops_total`          | counter   | -                                                                 |

The request metrics' `endpoint` label includes `llm/chat/completions` and
`llm/models`, the LLM proxy's routes. Their
`cm_agent_webhook_request_duration_seconds` observations span whole streams,
often longer than the histogram's top bucket, so filter latency alerts by
endpoint. `code="aborted"` counts a handler that panicked, for example the
proxy aborting a stream after the worker dropped it.

Deferred: panic-recovery counting and OTEL tracing.
