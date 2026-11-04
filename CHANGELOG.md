# Changelog

User-visible changes. GitHub release notes are generated from the merged pull
requests (see [docs/release.md](docs/release.md)); this file collects what a
user needs to know across them.

## Unreleased

### Added
- ate-env-api: `GetImageStatus` and `EnsureImageTemplate`, the ActorTemplate
  derived for a `rootfs_image` and the state of its golden snapshot (`NONE`,
  `PENDING`, `READY` with the tag, `FAILED` with the reason); ensure creates
  the template, which starts the bake, with no environment. Feature
  `image_status`; `ate-env image-status <image> [--ensure]` (`image-status` joins
  the reserved environment ids); the Python
  client's `image_status()` and `ImageStatus`; the Go client's `ImageStatus`
  and `EnsureImageTemplate`.
- Python client: `FleetConfig.node_selector` is sent as every environment's
  `worker_selector`, and `FleetConfig.placement` adds labels per task image;
  `SubstrateBackendDriver(worker_selector=, placement=)` and
  `worker_selector_for(image)`.
- Python client: `ate_env.PreflightWarmer` fills the node caches an image
  placement plan (`ImagePlacement`, from `image-placement.json`) describes:
  ensures each image's derived template, then per domain, as each golden
  becomes ready, probes every node with the image and deletes the probes;
  `PreflightCoverage` writes `preflight-coverage.json` in the loader's schema.
  `Observer.record_phase()` and the phases `preflight_golden`,
  `preflight_probe`.
- ate-env-api: `worker_selector` on `CreateEnvironment`, labels the worker pool
  must carry, ANDed with the template's own selector (`Actor.worker_selector`,
  so it holds for wakes too); `node` and `worker_selector` on `Environment`.
  `ate-env create --worker-selector key=value`, `ate-env get <id>` (`get` joins
  the reserved environment ids);
  `worker_selector=` on the Python client's `create()` and `node`,
  `worker_selector` on `EnvironmentInfo`.
- Observability across the stack, all off by default except the run report;
  see [docs/observability.md](docs/observability.md).
- Python client: OpenTelemetry tracing behind the `tracing` extra
  (`TracerConfig`, `FleetConfig.tracing`, `ObservabilityConfig.enable_tracing`).
  `ate_env_client.*` and `ate_env_fleet.*` spans, W3C `traceparent` on every gRPC
  call, context carried across `LoopRunner` threads and `process_parallel`.
- Python client: `ate_env.observability`. `fleet.report` is a `RunReport`
  (per-phase count, total and max, claims, task outcomes, warm replicas) with
  `summary()`, `to_dict()` and `write(report_dir)`; `ate_env_fleet_*` and
  `ate_env_client_*` Prometheus series behind the `metrics` extra, served by
  `serve_metrics(port)`.
- ate-env-api: `--enable-tracing` (OTLP over `OTEL_EXPORTER_OTLP_*`, trace
  continued to ateapi and the guest) and `--metrics-listen-addr` (off by
  default, for example `:9090`) serving the `ate_env_api_*` families documented in
  [docs/metrics.md](docs/metrics.md).
- `ate-env manifest`: `--enable-tracing`, `--otlp-endpoint`, `--otlp-insecure`
  and `--metrics-port`.
- ate-env-guest: `--enable-tracing` and `--enable-metrics`
  (`ate_env_guest_process_duration_seconds`).
- Go client: `ClientOptions.TracerProvider` and `TraceServiceName`,
  `NewTracerProvider`, the `Attr*` span attribute keys, and `Client.Get`.
- swebench-loader: `run -enable-tracing` records every event as a span under one
  `swebench-loader.run` span; the JSONL is unchanged.
- `examples/grpo` and the NeMo Gym rollout benchmark record their phases with
  the client's `Observer` and write the run report to `REPORT_DIR`.
  `examples/grpo/report.py` also reads that report; it keeps reading the old
  per-event JSONL for one more release.
- `benchmarking/telemetry_report`: builds a self-contained HTML report from run
  reports, OTLP/JSON spans and Prometheus or collector metrics.

### Changed

- Python client: `warm_pool` pools the actors it created before it raises
  `SandboxStartError`. An overlapping `warm_pool(wait=True)` counts the actors
  another call is still creating and can return before the pool is full.
- Python client: `unwarm_pool`, and a `resize_pool` that shrinks the pool,
  cancel `warm_pool` calls in flight for that pool: those calls delete the
  actors they create instead of pooling them.

### Fixed

- Python client: environments no longer outlive `teardown()`. A recycle,
  `warm_pool` call or failed warm claim that overlaps a teardown deletes its
  environment instead of pooling it, and the warm-replica gauge stays equal to
  the warm pool. An environment whose delete fails stays owned, so the next
  `reap` deletes it.
