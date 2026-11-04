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

func (c *Config) strategyPasses() int {
	if c.Mode == ModeSteps && c.Steps > 1 {
		return c.Steps
	}
	return 1
}

type strategyBatch struct {
	images []string
	envs   []*environment
}

func (r *runner) planStrategyPass(pass int, tp *trajectoryPicker) ([]*environment, []strategyBatch, error) {
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
			base := EnvID(r.cfg.RunID, in.InstanceID, k)
			id := base
			if pass > 1 {
				id = fmt.Sprintf("%s-s%d", base, pass)
			}
			e := &environment{
				id:            id,
				base:          base,
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

// runStrategy executes Config.Strategy ("none", "naive", "sliding", or
// "pipelined") with 1:1 lifecycle and working-set telemetry parity with
// Python SandboxFleet.
func (r *runner) runStrategy(ctx context.Context) error {
	tp := r.newTrajectoryPicker()
	passes := r.cfg.strategyPasses()

	var (
		statsMu          sync.Mutex
		hits             int
		misses           int
		created          int
		reused           int
		retired          int
		curWarm          int
		peakWarm         int
		prefetchMarginMS float64
	)

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

	warmBatch := func(wctx context.Context, batch []*environment, stepNum int) {
		forEach(wctx, len(batch), r.cfg.Width, func(i int) {
			if !acquireSlot(wctx) {
				return
			}
			defer releaseSlot()
			e := batch[i]
			// Pass 0 so r.create preserves e.id (which already carries -s<pass> when pass > 1).
			if !r.createWithStepEvent(wctx, e, stepNum) {
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
			statsMu.Lock()
			created++
			curWarm++
			if curWarm > peakWarm {
				peakWarm = curWarm
			}
			statsMu.Unlock()
		})
	}

	execWarmBatch := func(ectx context.Context, batch []*environment, stepNum int) int {
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
			statsMu.Lock()
			hits++
			reused++
			if curWarm > 0 {
				curWarm--
			}
			statsMu.Unlock()

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
					statsMu.Lock()
					retired++
					statsMu.Unlock()
				}
			}
		})
		return live
	}

	cleanupRemaining := func(envs []*environment, stepNum int) {
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
					statsMu.Lock()
					retired++
					if curWarm > 0 {
						curWarm--
					}
					statsMu.Unlock()
				}
			}
		})
	}

	for pass := 1; pass <= passes; pass++ {
		if ctx.Err() != nil {
			break
		}
		stepNum := 0
		if r.cfg.Mode == ModeSteps {
			stepNum = pass
		}
		envs, batches, err := r.planStrategyPass(pass, tp)
		if err != nil {
			return err
		}

		passStart := time.Now()
		var passLive int

		switch r.cfg.Strategy {
		case StrategyNone:
			var liveMu sync.Mutex
			forEach(ctx, len(envs), r.cfg.Width, func(i int) {
				e := envs[i]
				e.wsFresh = true
				e.wsWake = false
				statsMu.Lock()
				misses++
				statsMu.Unlock()
				if r.createWithStepEvent(ctx, e, stepNum) {
					statsMu.Lock()
					created++
					statsMu.Unlock()
					r.replay(ctx, e, stepNum)
					if !e.failed {
						liveMu.Lock()
						passLive++
						liveMu.Unlock()
					}
				}
				if !r.cfg.Keep && e.sb != nil {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
					r.remove(cleanup, e, stepNum)
					cancel()
					if e.sb == nil {
						statsMu.Lock()
						retired++
						statsMu.Unlock()
					}
				}
			})

		case StrategyNaive:
			warmBatch(ctx, envs, stepNum)
			if ctx.Err() == nil {
				passLive = execWarmBatch(ctx, envs, stepNum)
			}
			cleanupRemaining(envs, stepNum)

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
				warmBatch(ctx, b.envs, stepNum)
				if ctx.Err() == nil {
					passLive += execWarmBatch(ctx, b.envs, stepNum)
				}
				cleanupRemaining(b.envs, stepNum)
			}

		case StrategyPipelined:
			if len(batches) > 0 {
				warmBatch(ctx, batches[0].envs, stepNum)
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
						warmBatch(ctx, nextEnvs, stepNum)
						prefetchDoneAt = time.Now()
					}()
				}
				r.log.Info("pipelined window",
					slog.Int("window", bIdx+1),
					slog.Int("windows", len(batches)),
					slog.Int("images", len(b.images)),
					slog.Int("tasks", len(b.envs)))
				passLive += execWarmBatch(ctx, b.envs, stepNum)
				batchDoneAt := time.Now()
				cleanupRemaining(b.envs, stepNum)

				if hasPrefetch {
					prefetchWG.Wait()
					if !prefetchDoneAt.IsZero() && batchDoneAt.After(prefetchDoneAt) {
						statsMu.Lock()
						prefetchMarginMS += ms(batchDoneAt.Sub(prefetchDoneAt))
						statsMu.Unlock()
					}
				}
			}
			cleanupRemaining(envs, stepNum)
		}

		if r.cfg.Mode == ModeSteps {
			var hitRate float64
			if total := hits + misses; total > 0 {
				hitRate = float64(hits) / float64(total)
			}
			r.rec.Record(Event{
				RunID:                r.cfg.RunID,
				Event:                EventBarrier,
				Step:                 stepNum,
				DurMS:                ms(time.Since(passStart)),
				OK:                   ctx.Err() == nil,
				Count:                passLive,
				WorkingSetHits:       hits,
				WorkingSetMisses:     misses,
				WorkingSetHitRate:    hitRate,
				StandingPoolSize:     peakWarm,
				Created:              created,
				Reused:               reused,
				Retired:              retired,
				PrefetchLeadMarginMS: prefetchMarginMS,
			})
		}
	}

	var hitRate float64
	if total := hits + misses; total > 0 {
		hitRate = float64(hits) / float64(total)
	}
	r.wsStats = WorkingSetStats{
		Hits:                 hits,
		Misses:               misses,
		HitRate:              hitRate,
		StandingPoolSize:     peakWarm,
		Created:              created,
		Reused:               reused,
		Retired:              retired,
		PrefetchLeadMarginMS: prefetchMarginMS,
	}
	return nil
}

// createWithStepEvent creates e.id on the backend and records EventCreate at
// stepNum without mutating e.id (which planStrategyPass already suffixed).
func (r *runner) createWithStepEvent(ctx context.Context, e *environment, stepNum int) bool {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.CreateTimeout)
	defer cancel()
	e.createStart = time.Now()
	sb, err := r.cfg.Backend.Create(cctx, e.id, e.inst.Image)
	ev := r.event(EventCreate, e, stepNum, time.Since(e.createStart), err)
	r.rec.Record(ev)
	if err != nil {
		e.failed = true
		task := r.event(EventTask, e, stepNum, 0, err)
		r.rec.Record(task)
		r.countTask(e, stepNum, false, nil)
		r.log.Warn("create failed", slog.String("env", e.id), slog.String("err", err.Error()))
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if derr := r.cfg.Backend.Delete(cleanup, e.id); derr != nil && !isNotFound(derr) {
			r.log.Warn("delete after failed create", slog.String("env", e.id), slog.String("err", derr.Error()))
		}
		return false
	}
	e.sb = sb
	return true
}
