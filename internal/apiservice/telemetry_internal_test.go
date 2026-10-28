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

package apiservice

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/telemetry"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/metadata"
)

func TestRequestedAt(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	past := now.Add(-1500 * time.Millisecond)
	cases := []struct {
		name string
		md   metadata.MD
		want time.Time
	}{
		{"no metadata", nil, now},
		{"no header", metadata.Pairs("x-env-id", "a"), now},
		{"valid", metadata.Pairs(telemetry.RequestedAtHeader, past.Format(time.RFC3339Nano)), past},
		{"unparsable", metadata.Pairs(telemetry.RequestedAtHeader, "1500ms ago"), now},
		{"future", metadata.Pairs(telemetry.RequestedAtHeader, now.Add(time.Minute).Format(time.RFC3339Nano)), now},
	}
	for _, tc := range cases {
		ctx := context.Background()
		if tc.md != nil {
			ctx = metadata.NewIncomingContext(ctx, tc.md)
		}
		if got := requestedAt(ctx, now); !got.Equal(tc.want) {
			t.Errorf("%s: requestedAt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The pending map is bounded: when full, stale entries are dropped and a new
// one is admitted; with no stale entry the new one is not tracked.
func TestTrackCreateIsBounded(t *testing.T) {
	s := &Server{Metrics: telemetry.NewMetrics()}
	old := time.Now().Add(-2 * pendingTTL)
	for i := range maxPending {
		s.trackCreate(DefaultAtespace, string(rune('a'+i%26))+time.Duration(i).String(), old)
	}
	if len(s.pending) != maxPending {
		t.Fatalf("pending = %d, want %d", len(s.pending), maxPending)
	}
	s.trackCreate(DefaultAtespace, "fresh", time.Now())
	if len(s.pending) != 1 {
		t.Errorf("pending after eviction = %d, want 1 (only the fresh entry)", len(s.pending))
	}
	for i := range maxPending - 1 {
		s.trackCreate(DefaultAtespace, "x"+time.Duration(i).String(), time.Now())
	}
	s.trackCreate(DefaultAtespace, "overflow", time.Now())
	if _, ok := s.pending[DefaultAtespace+"/overflow"]; ok {
		t.Error("an entry was admitted past the bound with nothing stale to evict")
	}
}

// countEnvironments lists the atespaces concurrently but never more than
// limit at once, and counts each atespace's actors under its own name.
func TestCountEnvironmentsIsConcurrentAndBounded(t *testing.T) {
	const limit = 4
	var spaces []string
	for i := range maxAtespaces {
		spaces = append(spaces, fmt.Sprintf("space-%d", i))
	}
	var inFlight, peak atomic.Int32
	list := func(ctx context.Context, atespace string) ([]*ateapipb.Actor, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running := &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}}
		var i int
		fmt.Sscanf(atespace, "space-%d", &i)
		actors := make([]*ateapipb.Actor, i%3)
		for j := range actors {
			actors[j] = running
		}
		return actors, nil
	}

	start := time.Now()
	counts, err := countEnvironments(context.Background(), spaces, limit, list)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if p := peak.Load(); p > limit || p < 2 {
		t.Errorf("peak concurrent ListActors = %d, want between 2 and %d", p, limit)
	}
	// Sequential would take maxAtespaces * 20ms = 1.28s.
	if elapsed > time.Duration(maxAtespaces)*20*time.Millisecond/2 {
		t.Errorf("took %v, want the atespaces listed concurrently", elapsed)
	}
	if want := len(spaces) * len(environmentStates); len(counts) != want {
		t.Fatalf("got %d samples, want %d", len(counts), want)
	}
	for _, c := range counts {
		var i int
		fmt.Sscanf(c.Atespace, "space-%d", &i)
		want := 0
		if c.State == "running" {
			want = i % 3
		}
		if c.Count != want {
			t.Errorf("%s/%s = %d, want %d", c.Atespace, c.State, c.Count, want)
		}
	}
}

// The first ListActors error is returned and cancels the calls in flight.
func TestCountEnvironmentsReturnsFirstError(t *testing.T) {
	boom := errors.New("boom")
	list := func(ctx context.Context, atespace string) ([]*ateapipb.Actor, error) {
		if atespace == "bad" {
			return nil, boom
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, nil
		}
	}
	start := time.Now()
	_, err := countEnvironments(context.Background(), []string{"a", "b", "bad", "c", "d"}, 3, list)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v: the error did not cancel the calls in flight", d)
	}
}
