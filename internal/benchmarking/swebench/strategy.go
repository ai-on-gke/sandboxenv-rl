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
	"log/slog"
	"sync"
	"time"
)

// Strategy selects the warm-pool orchestration strategy for sandbox lifecycles,
// matching the four strategies in the Python SDK's SandboxFleet.
type Strategy string

const (
	// StrategyNone acquires sandboxes on demand (1:1 cold create -> replay -> delete)
	// with zero pre-warmed standing pool.
	StrategyNone Strategy = "none"
	// StrategyNaive pre-warms (creates and suspends) all task images up front,
	// replays all rollouts in parallel from the warm pool, and deletes on release.
	StrategyNaive Strategy = "naive"
	// StrategySliding warms only a bounded window of distinct images at a time,
	// replays that window's rollouts, and tears down before advancing.
	StrategySliding Strategy = "sliding"
	// StrategyPipelined overlaps background pre-warming of window N+1 while
	// window N rollouts execute (double-buffered sliding window).
	StrategyPipelined Strategy = "pipelined"
)

// AllStrategies lists every concrete Strategy in canonical sweep order.
var AllStrategies = []Strategy{
	StrategyNone,
	StrategyNaive,
	StrategySliding,
	StrategyPipelined,
}

// ValidateStrategy checks that s is empty (unset) or a supported Strategy.
func ValidateStrategy(s Strategy) error {
	switch s {
	case "", StrategyNone, StrategyNaive, StrategySliding, StrategyPipelined:
		return nil
	default:
		return fmt.Errorf("unknown strategy %q (want none, naive, sliding, or pipelined)", s)
	}
}

// DefaultWindowSize chooses a bounded sliding/pipelined image window when
// Config.WindowSize is 0: at most 8 images (matching FleetConfig.batch_size),
// and at most ceil(numImages/2) for smaller smoke runs so multi-window sliding
// and double-buffered prefetch are still exercised when numImages >= 2.
func DefaultWindowSize(numImages int) int {
	if numImages <= 1 {
		return 1
	}
	return min(8, max(1, (numImages+1)/2))
}

func (c *Config) distinctImages() []string {
	seen := make(map[string]bool, len(c.Instances))
	out := make([]string, 0, len(c.Instances))
	for _, in := range c.Instances {
		key := in.Image
		if key == "" {
			key = in.InstanceID
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func (c *Config) effectiveWindowSize() int {
	if c.WindowSize > 0 {
		return c.WindowSize
	}
	return DefaultWindowSize(len(c.distinctImages()))
}

func (c *Config) strategyFanout() int {
	if c.Mode == ModeSteps {
		return c.Rollouts
	}
	return c.TasksPerImage
}

// strategyPark is the park policy a strategy run reports: none deletes each
// environment after its replay, the others suspend into the warm pool.
func (c *Config) strategyPark() Park {
	if c.Strategy == StrategyNone {
		return ParkDelete
	}
	return ParkSuspend
}

type strategyBatch struct {
	images []string
	envs   []*environment
}

// planStrategy builds the run's environments, one per instance and rollout,
// and groups them by image into windows of effectiveWindowSize images. A
// strategy runs one pass (validate refuses more), so each environment keeps
// its EnvID name.
func (r *runner) planStrategy(tp *trajectoryPicker) ([]*environment, []strategyBatch, error) {
	fanout := r.cfg.strategyFanout()
	envs := make([]*environment, 0, len(r.cfg.Instances)*fanout)
	byImage := make(map[string][]*environment, len(r.cfg.Instances))
	var imageOrder []string

	for _, in := range r.cfg.Instances {
		imgKey := in.Image
		if imgKey == "" {
			imgKey = in.InstanceID
		}
		if _, ok := byImage[imgKey]; !ok {
			imageOrder = append(imageOrder, imgKey)
		}
		for k := 0; k < fanout; k++ {
			traj, err := tp.next(in)
			if err != nil {
				return nil, nil, err
			}
			id := EnvID(r.cfg.RunID, in.InstanceID, k)
			e := &environment{
				id:            id,
				base:          id,
				inst:          in,
				task:          k,
				traj:          traj,
				hasWSOverride: true,
				wsFresh:       true,
			}
			envs = append(envs, e)
			byImage[imgKey] = append(byImage[imgKey], e)
		}
	}

	window := r.cfg.effectiveWindowSize()
	if window < 1 {
		window = 1
	}
	var batches []strategyBatch
	for start := 0; start < len(imageOrder); start += window {
		end := min(start+window, len(imageOrder))
		bImgs := imageOrder[start:end]
		var bEnvs []*environment
		for _, img := range bImgs {
			bEnvs = append(bEnvs, byImage[img]...)
		}
		batches = append(batches, strategyBatch{
			images: bImgs,
			envs:   bEnvs,
		})
	}
	return envs, batches, nil
}

// warmPoolStats is the working-set bookkeeping of a strategy run. An
// environment is in the warm pool from its suspend in warmBatch until the
// claim that replays it or the delete that retires it, whichever comes
// first; environment.inWarmPool records which, so a delete after a claim
// (or after a failed suspend) does not count the environment out a second
// time and lower the standing pool a concurrent prefetch is building.
type warmPoolStats struct {
	mu               sync.Mutex
	hits             int
	misses           int
	created          int
	reused           int
	retired          int
	cur              int
	peak             int
	prefetchMarginMS float64
}

// warmed counts a suspended environment into the pool.
func (s *warmPoolStats) warmed(e *environment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
	e.inWarmPool = true
	s.cur++
	if s.cur > s.peak {
		s.peak = s.cur
	}
}

// claimed counts a warm hit: e leaves the pool for its replay.
func (s *warmPoolStats) claimed(e *environment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits++
	s.reused++
	s.leaveLocked(e)
}

// missed counts a cold claim, and coldCreated its create.
func (s *warmPoolStats) missed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.misses++
}

func (s *warmPoolStats) coldCreated() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
}

// retire counts a delete; an environment still in the pool leaves it.
func (s *warmPoolStats) retire(e *environment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired++
	s.leaveLocked(e)
}

func (s *warmPoolStats) leaveLocked(e *environment) {
	if e.inWarmPool {
		e.inWarmPool = false
		s.cur--
	}
}

func (s *warmPoolStats) addPrefetchMargin(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prefetchMarginMS += ms(d)
}

func (s *warmPoolStats) snapshot() WorkingSetStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hitRate float64
	if total := s.hits + s.misses; total > 0 {
		hitRate = float64(s.hits) / float64(total)
	}
	return WorkingSetStats{
		Hits:                 s.hits,
		Misses:               s.misses,
		HitRate:              hitRate,
		StandingPoolSize:     s.peak,
		Created:              s.created,
		Reused:               s.reused,
		Retired:              s.retired,
		PrefetchLeadMarginMS: s.prefetchMarginMS,
	}
}

// runStrategy executes Config.Strategy ("none", "naive", "sliding", or
// "pipelined") with 1:1 lifecycle and working-set telemetry parity with
// Python SandboxFleet.
func (r *runner) runStrategy(ctx context.Context) error {
	tp := r.newTrajectoryPicker()
	stepNum := 0
	if r.cfg.Mode == ModeSteps {
		stepNum = 1
	}
	envs, batches, err := r.planStrategy(tp)
	if err != nil {
		return err
	}

	var ws warmPoolStats
	sem := make(chan struct{}, max(1, r.cfg.Width))
	acquireSlot := func(sctx context.Context) bool {
		select {
		case sem <- struct{}{}:
			return true
		case <-sctx.Done():
			return false
		}
	}
	releaseSlot := func() { <-sem }

	warmBatch := func(wctx context.Context, batch []*environment) {
		forEach(wctx, len(batch), r.cfg.Width, func(i int) {
			if !acquireSlot(wctx) {
				return
			}
			defer releaseSlot()
			e := batch[i]
			if !r.create(wctx, e, stepNum) {
				return
			}
			sStart := time.Now()
			err := e.sb.Suspend(wctx)
			r.rec.Record(r.event(EventSuspend, e, stepNum, time.Since(sStart), err))
			if err != nil {
				e.failed = true
				r.rec.Record(r.event(EventTask, e, stepNum, 0, err))
				r.countTask(e, stepNum, false, nil)
				r.log.Warn("warm suspend failed", slog.String("env", e.id), slog.String("err", err.Error()))
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(wctx), cleanupTimeout)
				r.remove(cleanup, e, stepNum)
				cancel()
				return
			}
			e.wsFresh = true
			e.wsWake = true
			ws.warmed(e)
		})
	}

	execWarmBatch := func(ectx context.Context, batch []*environment) int {
		var live int
		var liveMu sync.Mutex
		forEach(ectx, len(batch), r.cfg.Width, func(i int) {
			e := batch[i]
			if e.failed || e.sb == nil {
				return
			}
			if !acquireSlot(ectx) {
				return
			}
			defer releaseSlot()
			ws.claimed(e)

			r.replay(ectx, e, stepNum)
			if !e.failed {
				liveMu.Lock()
				live++
				liveMu.Unlock()
			}
			if !r.cfg.Keep {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ectx), cleanupTimeout)
				r.remove(cleanup, e, stepNum)
				cancel()
				if e.sb == nil {
					ws.retire(e)
				}
			}
		})
		return live
	}

	// cleanupRemaining deletes what a window left behind: environments the
	// run never claimed (a cancelled window, a failed suspend) and ones
	// whose delete after the replay failed.
	cleanupRemaining := func(envs []*environment) {
		if r.cfg.Keep {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		forEach(cleanup, len(envs), r.cfg.Width, func(i int) {
			e := envs[i]
			if e.sb != nil {
				r.remove(cleanup, e, stepNum)
				if e.sb == nil {
					ws.retire(e)
				}
			}
		})
	}

	start := time.Now()
	var live int

	switch r.cfg.Strategy {
	case StrategyNone:
		var liveMu sync.Mutex
		forEach(ctx, len(envs), r.cfg.Width, func(i int) {
			e := envs[i]
			e.wsFresh = true
			e.wsWake = false
			ws.missed()
			if r.create(ctx, e, stepNum) {
				ws.coldCreated()
				r.replay(ctx, e, stepNum)
				if !e.failed {
					liveMu.Lock()
					live++
					liveMu.Unlock()
				}
			}
			if !r.cfg.Keep && e.sb != nil {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
				r.remove(cleanup, e, stepNum)
				cancel()
				if e.sb == nil {
					ws.retire(e)
				}
			}
		})

	case StrategyNaive:
		warmBatch(ctx, envs)
		if ctx.Err() == nil {
			live = execWarmBatch(ctx, envs)
		}
		cleanupRemaining(envs)

	case StrategySliding:
		for bIdx, b := range batches {
			if ctx.Err() != nil {
				break
			}
			r.log.Info("sliding window",
				slog.Int("window", bIdx+1),
				slog.Int("windows", len(batches)),
				slog.Int("images", len(b.images)),
				slog.Int("tasks", len(b.envs)))
			warmBatch(ctx, b.envs)
			if ctx.Err() == nil {
				live += execWarmBatch(ctx, b.envs)
			}
			cleanupRemaining(b.envs)
		}

	case StrategyPipelined:
		if len(batches) > 0 {
			warmBatch(ctx, batches[0].envs)
		}
		for bIdx, b := range batches {
			if ctx.Err() != nil {
				break
			}
			var (
				prefetchWG     sync.WaitGroup
				prefetchDoneAt time.Time
				hasPrefetch    bool
			)
			if bIdx+1 < len(batches) {
				hasPrefetch = true
				nextEnvs := batches[bIdx+1].envs
				prefetchWG.Add(1)
				go func() {
					defer prefetchWG.Done()
					warmBatch(ctx, nextEnvs)
					prefetchDoneAt = time.Now()
				}()
			}
			r.log.Info("pipelined window",
				slog.Int("window", bIdx+1),
				slog.Int("windows", len(batches)),
				slog.Int("images", len(b.images)),
				slog.Int("tasks", len(b.envs)))
			live += execWarmBatch(ctx, b.envs)
			batchDoneAt := time.Now()
			cleanupRemaining(b.envs)

			if hasPrefetch {
				prefetchWG.Wait()
				if !prefetchDoneAt.IsZero() && batchDoneAt.After(prefetchDoneAt) {
					ws.addPrefetchMargin(batchDoneAt.Sub(prefetchDoneAt))
				}
			}
		}
		cleanupRemaining(envs)
	}

	r.wsStats = ws.snapshot()
	if r.cfg.Mode == ModeSteps {
		r.rec.Record(Event{
			RunID:                r.cfg.RunID,
			Event:                EventBarrier,
			Step:                 stepNum,
			DurMS:                ms(time.Since(start)),
			OK:                   ctx.Err() == nil,
			Count:                live,
			WorkingSetHits:       r.wsStats.Hits,
			WorkingSetMisses:     r.wsStats.Misses,
			WorkingSetHitRate:    r.wsStats.HitRate,
			StandingPoolSize:     r.wsStats.StandingPoolSize,
			Created:              r.wsStats.Created,
			Reused:               r.wsStats.Reused,
			Retired:              r.wsStats.Retired,
			PrefetchLeadMarginMS: r.wsStats.PrefetchLeadMarginMS,
		})
	}
	return nil
}
