# Decisions and deviations log

Append-only. Each entry: date, who, what, why.

- 2026-10-29, owner: the goal is one central website for all users of the clusters, always current
  on every ate-env deployment across all namespaces and all clusters. The per-run explorer plan
  (`plans/rl-perf-explorer/`) does not cover deployment, multi-cluster, auth or tenancy; its
  Sandboxes view and restore models carry over as a page of the new site.
- 2026-10-30, owner: read-only. No action from the site reaches a cluster.
- 2026-10-30, owner: the site runs on Cloud Run. Consequences: no long-running poller in the site;
  subscriptions and Eventarc replace polling; clusters push, the site reads.
- 2026-10-30, owner and orchestrator: GCS is the one store the project owns, for inventory
  snapshots, event archive, raw spans and run artifacts. Time series stay in Cloud Monitoring and
  traces in Cloud Trace, because re-aggregating scraped files into charts or indexing millions of
  span lines would re-implement Prometheus and a trace backend.
- 2026-10-30, orchestrator: events are queried from BigQuery, filled by a Pub/Sub BigQuery
  subscription, with a Cloud Storage subscription as the archive. GCS alone cannot append and
  batches files on a window of at least a minute, which misses the seconds target for the live feed.
  The owner can reverse this to GCS-only at the cost of a minute of event latency.
- 2026-10-30, orchestrator: inventory is not derived from traces. Tracing is opt-in and off by
  default, idle and suspended actors emit no spans, and WorkerPools, templates and deployments have
  none. Traces stay the drilldown.
- 2026-10-30, orchestrator, from research: the source of transitions is Substrate's actor event
  stream (`Actor state changed`, `Actor crashed`), one record per committed transition with the
  actor uid, the operation, the state and the trace id. Phase 1 reads it from pod stdout through a
  Cloud Logging sink, which needs no Substrate change; phase 2 switches `ate-otel-config` to
  `OTEL_LOGS_EXPORTER=otlp,console` and routes by `event.name` in the collector. The Substrate
  document's rules apply: never sample or filter the stream; take the last record per uid; reconcile
  with the control plane.
- 2026-10-30, orchestrator, from research: the reporter stays, for current state and for Kubernetes
  objects. `ateapi` has no watch RPC; `ListActors` across atespaces paged at 1000 every 5 s is the
  poll. It needs a global AccessPolicy with the `viewer` role, which exists in `ateapi`'s authz next
  to `editor` and `owner`.
- 2026-10-30, orchestrator, from research: the trace join through Substrate needs no new mechanism.
  The router ingress extracts `traceparent` and resumes under it; `ateapi` stamps actor identity on
  its server span and opens a span per workflow step; atelet and ateworker continue it. This closes
  the telemetry plan's open question.
- 2026-10-30, orchestrator: push reporters per cluster rather than a central poller, so the
  internet-facing service holds no cluster credentials.
- 2026-10-30, orchestrator: a thin custom Go service rather than Grafana. Grafana needs a database
  and does not fit Cloud Run; depth links go to Cloud Monitoring dashboards.
- 2026-10-30, orchestrator, tentative: reporter, schema, site, manifests and Terraform live in
  `ai-on-gke/sandboxenv-rl` because the reporter reuses `internal/ate` and the site reuses the
  schema package. The alternative is a separate repository; the maintainers decide at the first pull
  request.
- 2026-10-30, orchestrator, tentative: the site is Go with the standard library, server-rendered
  pages, inline SVG, no external assets, in the style of the explorer plan and `build_report.py`.
- 2026-10-30, open: the central project, the bucket name, the Google group, the cluster list,
  whether Cloud Trace is enabled in each cluster project, and whether every user sees every
  namespace.
- 2026-11-02, orchestrator: the plan is proposed to `ai-on-gke/sandboxenv-rl` under
  `docs/plans/sandbox-fleet-site/`, as the telemetry plan was in #189, so maintainers can object to
  the design before the first code pull request. The notes repository keeps the 2026-10-30 draft.
- 2026-11-03, orchestrator: the schema package is `internal/inventory`, not the planned
  `internal/fleet/v1`. "Fleet" already names the Python `SandboxFleet` and the trainer-side
  `ate_env_fleet_*` series; the snapshot is an inventory of a cluster. The package landed as pull
  request 1 of the execution plan.
