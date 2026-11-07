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

package measure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// progressKube answers `get pods` with one more Ready worker per poll until
// it reaches max, then stops progressing.
type progressKube struct {
	polls int
	max   int
}

func readyPod(name string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"ns","creationTimestamp":"2026-10-27T00:00:00Z","labels":{"ate-env/workerpool":"wp"}},"spec":{"containers":[{"name":"ateworker"}]},"status":{"phase":"Running","conditions":[
		{"type":"PodScheduled","status":"True","lastTransitionTime":"2026-10-27T00:00:01Z"},
		{"type":"Initialized","status":"True","lastTransitionTime":"2026-10-27T00:00:02Z"},
		{"type":"ContainersReady","status":"True","lastTransitionTime":"2026-10-27T00:00:05Z"},
		{"type":"Ready","status":"True","lastTransitionTime":"2026-10-27T00:00:05Z"}]}}`, name)
}

func (p *progressKube) Run(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "get pods") {
		return []byte("ok"), nil
	}
	p.polls++
	n := min(p.polls, p.max)
	pods := make([]string, 0, n)
	for i := 0; i < n; i++ {
		pods = append(pods, readyPod(fmt.Sprintf("wp-%d", i)))
	}
	return []byte(`{"items":[` + strings.Join(pods, ",") + `]}`), nil
}

func TestSpinUpStallReturnsPartialMetrics(t *testing.T) {
	// Three pods come up, then nothing: the stall timeout fires long before
	// the overall timeout, and the three pods' metrics survive.
	kube := &progressKube{max: 3}
	metrics, err := SpinUpWorkerPoolWithOptions(context.Background(), kube, SpinUpOptions{
		Namespace: "ns", WorkerPool: "wp", Replicas: 10,
		Timeout: time.Hour, StallTimeout: 60 * time.Millisecond, Poll: 5 * time.Millisecond,
	})
	var te *SpinUpTimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want *SpinUpTimeoutError", err)
	}
	if !te.Stalled || te.Got != 3 || te.Want != 10 {
		t.Fatalf("unexpected timeout error: %+v", te)
	}
	if !strings.Contains(err.Error(), "stalled") || !strings.Contains(err.Error(), "3/10") {
		t.Fatalf("message should say stalled with counts: %v", err)
	}
	if metrics == nil {
		t.Fatal("partial metrics must be returned on stall")
	}
	if metrics["worker_pod_count"].P50 != 3 || metrics["worker_pod_missing"].P50 != 7 {
		t.Fatalf("counts = %+v / %+v", metrics["worker_pod_count"], metrics["worker_pod_missing"])
	}
	if metrics["worker_pod_total_ready_ms"].Count != 3 {
		t.Fatalf("per-pod phases must cover the Ready pods: %+v", metrics["worker_pod_total_ready_ms"])
	}
	if metrics["workerpool_spinup_wall_ms"].P50 <= 0 {
		t.Fatal("wall time must be recorded even on stall")
	}
}

func TestSpinUpSlowButProgressingFinishesUnderStallTimeout(t *testing.T) {
	// One pod per poll with a stall timeout much longer than a poll: never
	// stalls, completes at the target.
	kube := &progressKube{max: 8}
	metrics, err := SpinUpWorkerPoolWithOptions(context.Background(), kube, SpinUpOptions{
		Namespace: "ns", WorkerPool: "wp", Replicas: 8,
		Timeout: time.Hour, StallTimeout: time.Second, Poll: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("progressing spin-up must not stall: %v", err)
	}
	if metrics["worker_pod_count"].P50 != 8 {
		t.Fatalf("worker_pod_count = %v", metrics["worker_pod_count"])
	}
	if _, ok := metrics["worker_pod_missing"]; ok {
		t.Fatal("worker_pod_missing must be absent on success")
	}
}

func TestSpinUpOverallTimeoutWithoutStallDetection(t *testing.T) {
	kube := &progressKube{max: 1}
	metrics, err := SpinUpWorkerPoolWithOptions(context.Background(), kube, SpinUpOptions{
		Namespace: "ns", WorkerPool: "wp", Replicas: 4,
		Timeout: 40 * time.Millisecond, Poll: 5 * time.Millisecond,
	})
	var te *SpinUpTimeoutError
	if !errors.As(err, &te) || te.Stalled {
		t.Fatalf("want overall timeout error, got %v", err)
	}
	if metrics == nil || metrics["worker_pod_count"].P50 != 1 {
		t.Fatalf("partial metrics = %+v", metrics)
	}
}
