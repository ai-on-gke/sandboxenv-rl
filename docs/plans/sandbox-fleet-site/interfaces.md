# Interface contracts: sandbox fleet site

Status: draft, 2026-10-30. Names below are proposals until the first pull request lands; the lead
confirms them and records changes in [decisions.md](./decisions.md).

## Bucket layout

```
gs://<bucket>/v1/inventory/<project>/<cluster>/latest.json          overwritten every interval
gs://<bucket>/v1/inventory/<project>/<cluster>/snapshots/<ts>.json  hourly, lifecycle 30 days
gs://<bucket>/v1/events/<dt=YYYY-MM-DD>/...                          Cloud Storage subscription output, lifecycle 400 days
gs://<bucket>/v1/traces/<year>/<month>/<day>/...                     google_cloud_storage exporter output, gzip
gs://<bucket>/v1/runs/<run_id>/                                      RunReport .txt and .json, report.html, spans.jsonl
```

The site discovers clusters by listing `v1/inventory/*/*/latest.json`. There is no shared index
object, so no two writers touch one object.

## Snapshot schema, `internal/inventory`

```json
{
  "schema_version": 1,
  "generated_at": "2026-10-30T12:00:05Z",
  "project": "<gcp project>",
  "cluster": "<gke cluster>",
  "location": "<region>",
  "reporter": { "version": "v0.x.y", "commit": "abc1234", "interval_s": 5 },
  "atespaces": [{ "name": "ate-env", "create_time": "..." }],
  "templates": [
    {
      "atespace": "ate-env",
      "name": "default-template",
      "base": "",
      "guest_image": "...",
      "rootfs_image": "",
      "workerpool": "default-template-workerpool",
      "create_time": "..."
    }
  ],
  "tags": [{ "atespace": "ate-env", "name": "...", "template": "...", "create_time": "..." }],
  "workerpools": [
    {
      "namespace": "ate-env",
      "name": "default-template-workerpool",
      "replicas": 4,
      "ready_replicas": 4,
      "sandbox_class": "gvisor",
      "sandbox_config": "gvisor-default",
      "worker_image": "..."
    }
  ],
  "workers": [
    {
      "name": "<opaque>",
      "namespace": "ate-env",
      "pool": "default-template-workerpool",
      "pod": "...",
      "node": "...",
      "state": "idle",
      "capacity": { "cpu_m": 8000, "memory_bytes": 0 },
      "allocated": { "cpu_m": 2000, "memory_bytes": 0 }
    }
  ],
  "actors": [
    {
      "uid": "8f2a...",
      "atespace": "ate-env",
      "name": "env-...",
      "template": "default-template-1a2b3c4d5e6f",
      "source_tag": "",
      "state": "running",
      "assigned_node": "gke-...",
      "worker": "<opaque>",
      "create_time": "...",
      "update_time": "...",
      "crash": null,
      "labels": { "ate-env/run-id": "..." }
    }
  ],
  "deployments": [
    {
      "namespace": "ate-env",
      "name": "ate-env-api",
      "image": "...",
      "version": "v0.x.y",
      "ready": 1,
      "tracing": true,
      "metrics_port": 9090
    }
  ],
  "counts": {
    "actors_by_state": { "running": 12, "suspended": 40 },
    "actors_by_atespace": { "ate-env": 52 }
  }
}
```

Rules: timestamps RFC 3339 UTC; `state` is the `ateapi.ActorState` value lowercased, the same
vocabulary as the event stream; `worker` and `name` of workers are opaque control-plane names, never
parsed; labels pass through except keys under `ate.`; no key named `token`, `password` or
`authorization` anywhere.

## Reporter, `cmd/ate-env-reporter`

| Flag                                                        | Default                                 | Meaning                                                                                            |
| ----------------------------------------------------------- | --------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `-bucket`                                                   | required                                | destination bucket                                                                                 |
| `-project`, `-cluster`, `-location`                         | from the GKE metadata server when empty | snapshot identity                                                                                  |
| `-interval`                                                 | `5s`                                    | poll and write interval; backs off to 60 s on consecutive errors                                   |
| `-snapshot-interval`                                        | `1h`                                    | how often `latest.json` is also kept under `snapshots/`                                            |
| `-ateapi-endpoint`, `-ateapi-token-file`, `-ateapi-ca-file` | as `ate-env-api`                        | Substrate control plane                                                                            |
| `-health-listen-addr`                                       | `:9091`                                 | `/healthz` and `/readyz`                                                                           |
| `-enable-tracing`, `-metrics-listen-addr`                   | off                                     | the same telemetry flags as `ate-env-api`; metric `ate_env_reporter_poll_duration_seconds{status}` |

Identity: Kubernetes ServiceAccount `ate-env-reporter` in namespace `ate-env-reporter` labeled
`ate.dev/api-access=true`; projected token with audience `api.ate-system.svc`; Workload Identity to
`ate-env-reporter@<project>.iam.gserviceaccount.com` with `roles/storage.objectCreator` on the
bucket; global AccessPolicy binding `viewer` to
`user:system:serviceaccount:ate-env-reporter:ate-env-reporter`; ClusterRole with `get`, `list`,
`watch` on `workerpools.ate.dev`, `sandboxconfigs.ate.dev`, `deployments.apps`, `services`, `pods`.

Behavior: one `ListActors` across atespaces and one `ListWorkers` per tick, paged at 1000;
`ListAtespaces`, `ListActorTemplates` and `ListTags` every tenth tick or on a template-name miss;
Kubernetes objects from informers. The tick writes `latest.json` with a single atomic object write
and skips the write when nothing changed except `generated_at`, in which case it updates
`generated_at` through a metadata-only write at most every interval. The reporter never calls
`ate-env-api`.

## Event stream

Substrate's records as documented in its `docs/observability.md`:

| Record                | `event.name`              | Attributes                                                                                                                                                            |
| --------------------- | ------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Actor state changed` | `ate.actor.state_changed` | `ate.atespace`, `ate.actor.name`, `ate.actor.uid`, `ate.template.atespace`, `ate.template.name`, `ate.actor.operation.name`, `ate.actor.state`; `trace_id`, `span_id` |
| `Actor crashed`       | `ate.actor.crashed`       | the same                                                                                                                                                              |
| `Actor usage sampled` | `ate.actor.usage_sampled` | identity keys, `ate.workerpool.*`, `ate.sandbox.class`, `ate.stats.*`, `ate.actor.epoch`                                                                              |

Phase 1 sink filter, one sink per cluster project, destination the central topic:

```
resource.type="k8s_container"
resource.labels.namespace_name="ate-system"
resource.labels.container_name="ate-api-server"
(jsonPayload.msg="Actor state changed" OR jsonPayload.msg="Actor crashed")
```

BigQuery table `fleet.actor_events` from the Pub/Sub BigQuery subscription with the topic schema:
`publish_time`, `project`, `cluster`, `time`, `atespace`, `actor_name`, `actor_uid`,
`template_atespace`, `template_name`, `operation`, `state`, `trace_id`, `span_id`, `raw` (JSON). A
small Cloud Run function or Dataflow template is not needed: the subscription's "write metadata"
option and a topic schema carry these columns; the sink's log entry is the `raw` payload and the
project and cluster come from the log resource labels.

Phase 2: the collector receives the OTLP log events and routes `event.name` in
`{ate.actor.state_changed, ate.actor.crashed}` to `googlecloudpubsub` with no sampling and no
filtering processor on that pipeline, and `ate.actor.usage_sampled` to GCS only.

## Collector pipelines, `manifests/telemetry/collector.yaml`

| Pipeline | Receivers                                                                  | Exporters                                                                                                                                |
| -------- | -------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| traces   | `otlp` (4317, 4318)                                                        | `otlphttp` to `https://telemetry.googleapis.com`, `google_cloud_storage` (prefix `v1/traces`, gzip, partition `year=%Y/month=%m/day=%d`) |
| metrics  | `prometheus` scraping `ate-env-api` only when PodMonitoring is unavailable | `googlemanagedprometheus` or none                                                                                                        |
| logs     | `otlp`                                                                     | `googlecloudpubsub` for the two lifecycle events; `google_cloud_storage` for usage samples                                               |

The collector's service account holds `roles/cloudtrace.agent`, `roles/storage.objectCreator` on the
bucket and `roles/pubsub.publisher` on the topic.

## Site, `cmd/ate-env-fleet-site`

Go, standard library HTTP, server-rendered HTML with inline SVG and small inline scripts, no
external assets, the same zero-dependency style as the explorer plan and `build_report.py`.

| Route                                                    | Returns                                                  |
| -------------------------------------------------------- | -------------------------------------------------------- |
| `GET /`                                                  | Fleet page                                               |
| `GET /clusters/{project}/{cluster}`                      | Cluster page                                             |
| `GET /actors/{uid}`                                      | Actor page                                               |
| `GET /runs`, `GET /runs/{run_id}`                        | Runs                                                     |
| `GET /live`                                              | last 200 transitions                                     |
| `GET /api/v1/clusters`                                   | the index built from `latest.json` objects               |
| `GET /api/v1/clusters/{project}/{cluster}`               | that snapshot                                            |
| `GET /api/v1/actors/{uid}/events?since=`                 | rows from `actor_events`                                 |
| `GET /api/v1/metrics/{name}?cluster=&namespace=&window=` | a named PromQL query from an allow-list, never free-form |
| `GET /healthz`                                           | 200                                                      |

Auth: IAP in front; the service verifies `x-goog-iap-jwt-assertion` against the IAP audience on
every request and reads the user email from it. Service account roles: `roles/storage.objectViewer`
on the bucket, `roles/bigquery.dataViewer` and `roles/bigquery.jobUser` on the events dataset,
`roles/monitoring.viewer` on the metrics scope. No other role.

Caching: snapshots cached in memory per instance for 30 s and revalidated by object generation;
PromQL results cached 10 s; event queries bounded to 24 h by default.

## Named PromQL queries

| Name                  | Query                                                                                                                                                                                             |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `requests_rate`       | `sum by (cluster, namespace, method) (rate(ate_env_api_requests_total[1m]))`                                                                                                                      |
| `create_to_ready_p50` | `histogram_quantile(0.5, sum by (cluster, namespace, le) (rate(ate_env_api_create_to_ready_seconds_bucket[5m])))`                                                                                 |
| `wake_wait_p95`       | `histogram_quantile(0.95, sum by (cluster, namespace, le) (rate(ate_env_api_wake_wait_seconds_bucket[5m])))`                                                                                      |
| `environments`        | `sum by (cluster, namespace, atespace, state) (ate_env_api_environments)`                                                                                                                         |
| `workers`             | `sum by (cluster, ate_workerpool_namespace, ate_workerpool_name) (max by (cluster, ate_workerpool_namespace, ate_workerpool_name, ate_worker_state, ate_sandbox_class) (ate_workerpool_workers))` |
| `crashes_rate`        | `sum by (cluster, ate_template_name) (rate(ate_actor_crashes_total[5m]))`                                                                                                                         |

Metric names on the Cloud Monitoring side follow the OTLP-to-Prometheus mapping; the lead confirms
each name against the metrics explorer before the allow-list ships.

## Terraform, `deploy/fleet-site/`

Modules: `central` (bucket, lifecycle, topic, subscriptions, dataset, Cloud Run, IAP, service
accounts) applied once; `cluster` (sink, reporter and collector service accounts, Workload Identity
bindings) applied per cluster project. Inputs: `central_project`, `bucket`, `iap_group`,
`clusters = [{project, name, location}]`.
