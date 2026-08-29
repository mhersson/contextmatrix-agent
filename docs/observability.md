# Observability

`serve` exposes Prometheus metrics on a **separate, loopback-only admin
listener** - metrics never ride the public webhook port. `GET /metrics` on
`127.0.0.1:<admin_port>`, HMAC-signed with the same signed-GET scheme as the
webhook routes (sign `METHOD\nURI\nTS.BODY` with the backend `api_key`).
`admin_port: 0` (the default) disables the listener; the public port defaults
to `9092`, a typical admin port is `9093`. Env override: `CMX_ADMIN_PORT`.

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

Deferred: panic-recovery counting and OTEL tracing.
