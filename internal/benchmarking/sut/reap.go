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

package sut

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"strings"
	"time"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// DefaultReapPollInterval is the default polling cadence used by Reap when
// pollInterval is zero or negative.
const DefaultReapPollInterval = 200 * time.Millisecond

// LiveResourceListerDeleter queries the authoritative server or cluster API for
// live environments and deletes them by ID (enforcing Guardrail 3: server truth
// for reap; deleting only client-remembered handles is forbidden).
type LiveResourceListerDeleter interface {
	ListEnvironments(ctx context.Context, atespace string) ([]string, error)
	DeleteEnvironment(ctx context.Context, atespace, id string) error
}

// ATEClusterReaper implements LiveResourceListerDeleter directly against the
// Substrate ate-api control plane using ateapipb.ControlClient.
type ATEClusterReaper struct {
	Client ateapipb.ControlClient
}

// NewATEClusterReaper wraps an ateapipb.ControlClient as a LiveResourceListerDeleter.
func NewATEClusterReaper(client ateapipb.ControlClient) *ATEClusterReaper {
	return &ATEClusterReaper{Client: client}
}

// DialATEClusterReaper dials an ate-api gRPC control endpoint and returns an
// ATEClusterReaper along with its connection close function.
func DialATEClusterReaper(endpoint string) (*ATEClusterReaper, func() error, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		return nil, nil, err
	}
	return NewATEClusterReaper(ateapipb.NewControlClient(conn)), conn.Close, nil
}

// ListEnvironments queries ListActors on the Substrate control plane for all
// live actors in atespace and returns their sorted names.
func (r *ATEClusterReaper) ListEnvironments(ctx context.Context, atespace string) ([]string, error) {
	if r == nil || r.Client == nil {
		return nil, fmt.Errorf("ate cluster reaper: client is nil")
	}
	if atespace == "" {
		atespace = DefaultAtespace
	}
	resp, err := r.Client.ListActors(ctx, &ateapipb.ListActorsRequest{Atespace: atespace})
	if err != nil {
		return nil, fmt.Errorf("list actors in atespace %q: %w", atespace, err)
	}
	ids := make([]string, 0, len(resp.GetActors()))
	for _, a := range resp.GetActors() {
		if name := a.GetMetadata().GetName(); name != "" {
			ids = append(ids, name)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// DeleteEnvironment suspends (asynchronously) and deletes actor id in atespace
// on the Substrate control plane. NotFound is treated as already deleted.
func (r *ATEClusterReaper) DeleteEnvironment(ctx context.Context, atespace, id string) error {
	if r == nil || r.Client == nil {
		return fmt.Errorf("ate cluster reaper: client is nil")
	}
	if id == "" {
		return fmt.Errorf("ate cluster reaper: id is required")
	}
	if atespace == "" {
		atespace = DefaultAtespace
	}
	ref := &ateapipb.ObjectRef{
		Atespace: atespace,
		Name:     id,
	}
	if sp, ok := r.Client.(interface {
		SuspendActor(ctx context.Context, in *ateapipb.SuspendActorRequest, opts ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error)
	}); ok {
		_, err := sp.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
			Actor:   ref,
			Options: &ateapipb.SuspendOptions{Mode: ateapipb.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS},
		})
		if err != nil && status.Code(err) != codes.NotFound && status.Code(err) != codes.FailedPrecondition {
			return fmt.Errorf("suspend actor %s/%s before delete: %w", atespace, id, err)
		}
	}
	_, err := r.Client.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete actor %s/%s: %w", atespace, id, err)
	}
	return nil
}

// ReapResult summarizes a server-queried reap operation.
type ReapResult struct {
	Deleted            int                      `json:"deleted"`
	Remaining          int                      `json:"remaining"`
	RemainingIDs       []string                 `json:"remaining_ids,omitempty"`
	Duration           time.Duration            `json:"duration"`
	DeletionRatePerSec float64                  `json:"deletion_rate_per_sec"`
	DeleteDurations    map[string]time.Duration `json:"delete_durations,omitempty"`
}

// Reap queries backend.ListEnvironments for all live environments in atespace,
// filters those matching prefix (or all live environments when prefix == ""),
// deletes every matching environment via backend.DeleteEnvironment, and polls
// ListEnvironments until 0 matching environments remain on the server or ctx expires.
func Reap(ctx context.Context, backend LiveResourceListerDeleter, atespace, prefix string, pollInterval time.Duration) (*ReapResult, error) {
	if backend == nil {
		return nil, fmt.Errorf("reap: backend is required")
	}
	if atespace == "" {
		atespace = DefaultAtespace
	}
	if pollInterval <= 0 {
		pollInterval = DefaultReapPollInterval
	}

	start := time.Now()
	discovered := make(map[string]struct{})
	deleteDurs := make(map[string]time.Duration)
	var lastMatching []string
	var lastDeleteErr error

	for {
		live, err := backend.ListEnvironments(ctx, atespace)
		if err != nil {
			res := buildReapResult(start, discovered, lastMatching, deleteDurs)
			return res, fmt.Errorf("reap list environments: %w", err)
		}
		matching := filterByPrefix(live, prefix)
		for _, id := range matching {
			discovered[id] = struct{}{}
		}
		lastMatching = matching

		if len(matching) == 0 {
			return buildReapResult(start, discovered, nil, deleteDurs), nil
		}

		for _, id := range matching {
			t0 := time.Now()
			if err := backend.DeleteEnvironment(ctx, atespace, id); err != nil {
				lastDeleteErr = err
			} else if _, seen := deleteDurs[id]; !seen {
				deleteDurs[id] = time.Since(t0)
			}
		}

		// Re-check immediately after issuing deletes before waiting for pollInterval.
		liveAfter, err := backend.ListEnvironments(ctx, atespace)
		if err != nil {
			res := buildReapResult(start, discovered, lastMatching, deleteDurs)
			return res, fmt.Errorf("reap verify environments: %w", err)
		}
		matchingAfter := filterByPrefix(liveAfter, prefix)
		for _, id := range matchingAfter {
			discovered[id] = struct{}{}
		}
		lastMatching = matchingAfter
		if len(matchingAfter) == 0 {
			return buildReapResult(start, discovered, nil, deleteDurs), nil
		}

		select {
		case <-ctx.Done():
			res := buildReapResult(start, discovered, lastMatching, deleteDurs)
			if lastDeleteErr != nil {
				return res, fmt.Errorf("reap did not reach zero before context deadline (%d remaining: %v; last delete err: %v): %w",
					res.Remaining, res.RemainingIDs, lastDeleteErr, ctx.Err())
			}
			return res, fmt.Errorf("reap did not reach zero before context deadline (%d remaining: %v): %w",
				res.Remaining, res.RemainingIDs, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func filterByPrefix(ids []string, prefix string) []string {
	var out []string
	for _, id := range ids {
		if prefix == "" || strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func buildReapResult(start time.Time, discovered map[string]struct{}, remaining []string, deleteDurs map[string]time.Duration) *ReapResult {
	dur := time.Since(start)
	remSet := make(map[string]struct{}, len(remaining))
	for _, id := range remaining {
		remSet[id] = struct{}{}
	}
	deleted := 0
	for id := range discovered {
		if _, stillThere := remSet[id]; !stillThere {
			deleted++
		}
	}
	var rate float64
	if secs := dur.Seconds(); secs > 0 && deleted > 0 {
		rate = float64(deleted) / secs
	}
	var remCopy []string
	if len(remaining) > 0 {
		remCopy = append([]string(nil), remaining...)
	}
	return &ReapResult{
		Deleted:            deleted,
		Remaining:          len(remaining),
		RemainingIDs:       remCopy,
		Duration:           dur,
		DeletionRatePerSec: rate,
		DeleteDurations:    deleteDurs,
	}
}
