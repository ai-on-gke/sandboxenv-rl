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
	"unicode/utf8"

	"github.com/agent-substrate/env/internal/ate"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
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

// maxGoldenErrorInStatus bounds the golden error in a gRPC status message;
// Substrate allows 4096 bytes.
const maxGoldenErrorInStatus = 256

// imageTemplateLocks holds one imageTemplateLock for each derived template
// name this process has used. The set is bounded by the number of images.
type imageTemplateLocks struct {
	mu sync.Mutex
	m  map[string]*imageTemplateLock
}

// imageTemplateLock serializes the recreate of one derived template (write
// lock) with the creates that use it (read lock, held until the actor is
// created). Thus, in one process, no actor is created on a template while it
// is deleted and recreated, and each actor created before the recreate is
// seen by the in-use check.
type imageTemplateLock struct {
	mu sync.RWMutex
	// keptUID and keptAt record the failed template that was kept because
	// actors used it, so the next check of that template waits for
	// failedTemplateRecreateAfter. Written under the write lock.
	keptUID string
	keptAt  time.Time
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
// The template is kept when an actor other than its golden actor uses it. A
// recreate gives the template a new uid, and Substrate resumes a suspended
// actor whose snapshot names another template uid from its volumes only:
// such an actor would lose its rootfs and processes without an error. Only
// the failed template is deleted, by its uid; base is never deleted.
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
			return status.Errorf(codes.Unavailable,
				"actor template %q failed its golden snapshot (%s) and could not be deleted for a recreate: %v; retry the create",
				name, truncateUTF8(golden, maxGoldenErrorInStatus), err)
		}
		cur, err := s.getOrCreateImageTemplate(ctx, atespace, base, name, image)
		if err != nil {
			return err
		}
		if cur.GetMetadata().GetUid() != uid {
			return nil
		}
		if time.Now().After(deadline) {
			return status.Errorf(codes.Unavailable,
				"actor template %q failed its golden snapshot (%s) and is not deleted after %s; retry the create",
				name, truncateUTF8(golden, maxGoldenErrorInStatus), templateRecreateTimeout)
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-time.After(templateRecreatePoll):
		}
	}
}

// templateUsers counts the actors in actors that use template name in
// atespace, except its golden actor, which Substrate names after the
// template uid and deletes with the template.
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
		n++
	}
	return n
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
