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

package swebench

import (
	"context"
	"fmt"
	"sync/atomic"

	env "github.com/agent-substrate/env/clients/go"
	"github.com/agent-substrate/env/internal/benchmarking/rlbench"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
)

// Result is what a Run step returned.
type Result = rlbench.Result

// ExecResult aliases rlbench.ExecResult.
type ExecResult = rlbench.ExecResult

// Sandbox is the subset of an environment the loader drives.
type Sandbox = rlbench.Sandbox

// Backend creates sandboxes.
type Backend = rlbench.Backend

// EnvBackend is the Backend over the ate-env API through the Go SDK.
type EnvBackend struct {
	clients      []*env.Client
	next         atomic.Uint64
	atespace     string
	baseTemplate string
}

// NewEnvBackend dials the ate-env API over connections connections (at
// least one).
func NewEnvBackend(endpoint, atespace, baseTemplate string, connections int) (*EnvBackend, error) {
	if connections < 1 {
		connections = 1
	}
	b := &EnvBackend{atespace: atespace, baseTemplate: baseTemplate}
	for i := 0; i < connections; i++ {
		c, err := env.NewClient(env.ClientOptions{Endpoint: endpoint})
		if err != nil {
			b.Close()
			return nil, err
		}
		b.clients = append(b.clients, c)
	}
	return b, nil
}

// Close releases the API connections.
func (b *EnvBackend) Close() error {
	var first error
	for _, c := range b.clients {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// pick returns the next connection round-robin.
func (b *EnvBackend) pick() *env.Client {
	return b.clients[int(b.next.Add(1)-1)%len(b.clients)]
}

// Delete implements Backend.
func (b *EnvBackend) Delete(ctx context.Context, id string) error {
	return b.pick().Delete(ctx, b.atespace, id)
}

// Create implements Backend.
func (b *EnvBackend) Create(ctx context.Context, id, image string) (Sandbox, error) {
	e, err := b.pick().Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
		Id:          id,
		Atespace:    b.atespace,
		Template:    &ateenvv1alpha.Template{Name: b.baseTemplate, Atespace: b.atespace},
		RootfsImage: image,
	})
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", id, err)
	}
	return rlbench.NewEnvSandbox(e, pollMin, pollMax), nil
}

var (
	pollMin = rlbench.PollMin
	pollMax = rlbench.PollMax
)

const pollRetries = rlbench.PollRetries
