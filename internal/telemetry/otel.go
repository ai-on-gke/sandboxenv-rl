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

package telemetry

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Propagator is the only propagator the servers install: W3C Trace Context,
// without Baggage, so no caller-chosen key/value pairs ride along.
var Propagator propagation.TextMapPropagator = propagation.TraceContext{}

// Untraced returns ctx with a remote parent span context that is not
// sampled, so calls made with it are dropped by the default ParentBased
// samplers: here, and in ateapi, which receives the unsampled traceparent.
// Background reads such as the environments gauge use it so that each
// scrape does not start a new root trace.
func Untraced(ctx context.Context) context.Context {
	var tid trace.TraceID
	var sid trace.SpanID
	_, _ = rand.Read(tid[:])
	_, _ = rand.Read(sid[:])
	return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid,
		SpanID:  sid,
		Remote:  true,
		// TraceFlags zero: not sampled.
	}))
}

// SetupOTel installs a global OpenTelemetry tracer provider that exports
// spans over OTLP/gRPC and returns a function that flushes and shuts the
// provider down.
//
// The exporter is configured by the standard environment variables, chiefly
// OTEL_EXPORTER_OTLP_ENDPOINT (default localhost:4317) and
// OTEL_EXPORTER_OTLP_INSECURE; an http:// endpoint also means plaintext. The
// resource carries service.name = serviceName. The global propagator is set
// to W3C Trace Context only (no Baggage), which is what the otelgrpc stats
// handlers read and write.
func SetupOTel(ctx context.Context, serviceName string) (func(), error) {
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: creating OTLP trace exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", serviceName)))
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("telemetry: building resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(Propagator)

	shutdown := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(sctx)
	}
	return shutdown, nil
}
