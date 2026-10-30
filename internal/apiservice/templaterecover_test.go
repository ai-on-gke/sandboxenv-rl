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
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A create that waits for the recreate of its template (the write lock)
// gives up when its context ends, and takes the read lock once the recreate
// is done.
func TestImageTemplateLockReadHonorsContext(t *testing.T) {
	var l imageTemplateLock
	l.mu.Lock()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.rlock(ctx); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("rlock while the write lock is held: %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("rlock took %s, want it to end at the deadline", elapsed)
	}

	l.mu.Unlock()
	if err := l.rlock(t.Context()); err != nil {
		t.Fatalf("rlock after the write lock is released: %v", err)
	}
	l.mu.RUnlock()
}
