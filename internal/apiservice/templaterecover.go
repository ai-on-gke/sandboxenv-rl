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
	"log/slog"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// failedTemplateRecreateAfter is how long a derived template stays
	// after its golden snapshot build failed before a create recreates it.
	// A bake takes the warmup plus the wakeup probe (about 2 minutes), thus
	// an image whose bake keeps failing bakes at most once in this interval,
	// and an operator has time to see the ERROR template.
	failedTemplateRecreateAfter = 10 * time.Minute
	// templateRecreateTimeout bounds the wait for the replacement template
	// after the failed one is deleted.
	templateRecreateTimeout = 30 * time.Second
	// templateRecreatePoll is the wait between two reads in that loop.
	templateRecreatePoll = 200 * time.Millisecond
)

// lockPoll is the wait between two tries of a read lock; see rlock.
const lockPoll = 10 * time.Millisecond

// imageTemplateLocks holds one imageTemplateLock for each atespace and image
// that this process used. Entries are never removed: each is a few dozen
// bytes, thus even thousands of task images stay at kilobytes.
type imageTemplateLocks struct {
	mu sync.Mutex
	m  map[string]*imageTemplateLock
}

// imageTemplateLock serializes the recreate of one derived template (write
// lock) with the creates that use it (read lock, held until the actor is
// created). Thus, in one process, no actor is created on a template while it
// is deleted and recreated, and each actor created before the recreate is
// seen by the in-use check. Creates take the read lock with rlock, which
// gives up when the request's context ends; the recreate takes the write
// lock without a context, and waits only for the creates in flight, each one
// CreateActor call.
type imageTemplateLock struct {
	mu sync.RWMutex
	// keptUID and keptAt record the failed template that was kept (actors
	// use it, or the recreate failed), so the next check of that template
	// waits for failedTemplateRecreateAfter. Written under the write lock.
	keptUID string
	keptAt  time.Time
}

// rlock takes l.mu for reading, or returns the context error when ctx ends
// first. TryRLock fails while a writer waits, thus a pending recreate still
// goes before new creates.
func (l *imageTemplateLock) rlock(ctx context.Context) error {
	for !l.mu.TryRLock() {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(lockPoll):
		}
	}
	return nil
}

func (l *imageTemplateLocks) get(atespace, name string) *imageTemplateLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = make(map[string]*imageTemplateLock)
	}
	k := atespace + "/" + name
	lock, ok := l.m[k]
	if !ok {
		lock = &imageTemplateLock{}
		l.m[k] = lock
	}
	return lock
}

// recreateDue reports whether tmpl failed its golden snapshot build at least
// failedTemplateRecreateAfter ago, and was not kept for its actors in that
// interval. A template that is baking or ready is never due. The caller holds
// l.mu, in either mode.
func (l *imageTemplateLock) recreateDue(tmpl *ateapipb.ActorTemplate, now time.Time) bool {
	if tmpl.GetStatus().GetGoldenSnapshotStatus().GetErrorMessage() == "" {
		return false
	}
	failedAt := tmpl.GetMetadata().GetUpdateTime()
	if failedAt == nil {
		failedAt = tmpl.GetMetadata().GetCreateTime()
	}
	if failedAt == nil || now.Sub(failedAt.AsTime()) < failedTemplateRecreateAfter {
		return false
	}
	return l.keptUID != tmpl.GetMetadata().GetUid() || now.Sub(l.keptAt) >= failedTemplateRecreateAfter
}

// recreateFailedImageTemplate deletes the derived template name, whose golden
// snapshot build failed, and creates it again from base. Substrate does not
// retry a failed build (a nonempty golden_snapshot_status.error_message is
// terminal); creates on such a template still succeed, as cold boots without
// a snapshot.
//
// The template is kept when an actor uses it (see templateUsers). A recreate
// gives the template a new uid, and Substrate resumes a suspended actor whose
// snapshot names another template uid from its volumes only: such an actor
// would lose its rootfs and processes without an error. Only the failed
// template is deleted, by its uid; base is never deleted.
//
// When the recreate fails (the delete fails, or the old template is still
// there after templateRecreateTimeout), the template is kept for
// failedTemplateRecreateAfter and the create goes on as a cold boot on it,
// as it did before this recovery existed. An error is returned only when no
// template is left to create the actor on.
//
// The caller holds lock.mu for writing. Another ate-env-api replica does not
// hold that lock, thus it can create an actor on the failed template between
// the in-use check and the delete.
func (s *Server) recreateFailedImageTemplate(ctx context.Context, lock *imageTemplateLock, atespace, base, name, image string) error {
	failed, err := s.client.GetActorTemplate(ctx, atespace, name)
	if errors.Is(err, ate.ErrNotFound) {
		return nil
	}
	if err != nil {
		return toGRPCError(err)
	}
	now := time.Now()
	if !lock.recreateDue(failed, now) {
		return nil
	}
	uid := failed.GetMetadata().GetUid()
	golden := failed.GetStatus().GetGoldenSnapshotStatus().GetErrorMessage()

	actors, err := s.client.List(ctx, atespace)
	if err != nil {
		// The template stays usable for cold boots; try again later.
		lock.keptUID, lock.keptAt = uid, now
		slog.WarnContext(ctx, "keeping actor template with a failed golden snapshot: cannot list its actors",
			slog.String("template", name), slog.String("uid", uid), slog.String("golden_error", golden), slog.Any("err", err))
		return nil
	}
	if n := templateUsers(actors, atespace, name, uid); n > 0 {
		lock.keptUID, lock.keptAt = uid, now
		slog.WarnContext(ctx, "keeping actor template with a failed golden snapshot: actors use it; delete it when none does to rebake",
			slog.String("template", name), slog.String("uid", uid), slog.String("golden_error", golden), slog.Int("actors", n))
		return nil
	}

	slog.WarnContext(ctx, "recreating actor template with a failed golden snapshot",
		slog.String("template", name), slog.String("uid", uid), slog.String("golden_error", golden))
	deadline := now.Add(templateRecreateTimeout)
	for {
		// NotFound: the delete is done. Aborted: another delete, or the
		// template reconciler, holds the template's lease; the uid
		// precondition also answers Aborted for a template created again.
		// For each, the read below decides.
		err := s.client.DeleteActorTemplate(ctx, atespace, name, uid)
		if err != nil && !errors.Is(err, ate.ErrNotFound) && status.Code(err) != codes.Aborted {
			lock.keptUID, lock.keptAt = uid, now
			slog.WarnContext(ctx, "keeping actor template with a failed golden snapshot: cannot delete it; creates cold-boot on it",
				slog.String("template", name), slog.String("uid", uid), slog.String("golden_error", golden), slog.Any("err", err))
			return nil
		}
		cur, err := s.getOrCreateImageTemplate(ctx, atespace, base, name, image)
		if err != nil {
			return err
		}
		if cur.GetMetadata().GetUid() != uid {
			return nil
		}
		// The failed template is still there: a create can use it.
		if time.Now().After(deadline) {
			lock.keptUID, lock.keptAt = uid, now
			slog.WarnContext(ctx, "keeping actor template with a failed golden snapshot: still not deleted; creates cold-boot on it",
				slog.String("template", name), slog.String("uid", uid), slog.String("golden_error", golden),
				slog.Duration("waited", templateRecreateTimeout))
			return nil
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(templateRecreatePoll):
		}
	}
}

// templateUsers counts the actors in actors that use template name in
// atespace and that a recreate can harm. It skips:
//   - the golden actor, which Substrate names after the template uid and
//     deletes with the template;
//   - a CRASHED actor without a snapshot taken under uid. Substrate can
//     revert a CRASHED actor to its last durable snapshot and resume it, thus
//     a CRASHED actor with such a snapshot still counts.
//
// A RUNNING actor counts, although a suspend after the recreate is safe: in
// Substrate v0.6.0 the suspend resolves the template by name and stamps the
// snapshot with the uid of the template that exists then, the new one
// (workflow_suspend.go, ensureMarkedSuspending; snapshot_helpers.go,
// newDurableSnapshot). It counts because its earlier snapshots, under uid,
// stay its revert target.
func templateUsers(actors []*ateapipb.Actor, atespace, name, uid string) int {
	n := 0
	for _, a := range actors {
		ref := a.GetActorTemplate()
		if ref.GetName() != name || (ref.GetAtespace() != "" && ref.GetAtespace() != atespace) {
			continue
		}
		if a.GetMetadata().GetName() == uid {
			continue
		}
		if a.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED && !hasSnapshotUnder(a, uid) {
			continue
		}
		n++
	}
	return n
}

// hasSnapshotUnder reports whether a holds a snapshot taken under the
// template with uid.
func hasSnapshotUnder(a *ateapipb.Actor, uid string) bool {
	for _, snap := range a.GetStatus().GetSnapshots() {
		if snap.GetActorTemplateUid() == uid {
			return true
		}
	}
	return false
}
