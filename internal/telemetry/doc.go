// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package telemetry holds the tracing and metrics plumbing of the ate-env
// servers: the OpenTelemetry setup (SetupOTel, the W3C-only Propagator and
// Untraced) and the Prometheus families ate-env-api serves on its metrics
// listener (Metrics, Definitions).
//
// Nothing here has side effects at import: no global tracer provider is set
// and no collector is registered until SetupOTel or NewMetrics is called.
package telemetry
