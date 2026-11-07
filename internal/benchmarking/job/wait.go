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

package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// jobWaitTimeout is the default bound on how long a real job may run
// (JobSpec.timeout overrides it); jobPollInterval is how often its state is
// read. Both are variables so tests can shorten them.
var (
	jobWaitTimeout  = time.Hour
	jobPollInterval = 5 * time.Second
)

// ErrJobFailed is wrapped by the error returned when a Job or its trainer
// container ends unsuccessfully, so callers can tell a failed run from a
// wait that could not observe the Job at all.
var ErrJobFailed = errors.New("job failed")

type k8sJobStatus struct {
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

type k8sPodListStatus struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State struct {
					Terminated *struct {
						ExitCode int    `json:"exitCode"`
						Reason   string `json:"reason"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

func runStdout(ctx context.Context, exec CommandExecutor, env map[string]string, args ...string) ([]byte, error) {
	if so, ok := exec.(StdoutExecutor); ok {
		return so.Output(ctx, "kubectl", env, args...)
	}
	return exec.Run(ctx, "kubectl", env, args...)
}

// waitForJobTerminal polls `kubectl get job` until the Job carries a
// Complete or Failed condition. Complete returns nil; Failed returns an
// error wrapping ErrJobFailed at once, instead of letting
// `kubectl wait --for=condition=complete` sit on a dead Job until its
// timeout. A transient read error is retried until the deadline.
func waitForJobTerminal(ctx context.Context, exec CommandExecutor, env map[string]string, ns, name string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = jobWaitTimeout
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		raw, err := runStdout(ctx, exec, env, "-n", ns, "get", "job", name, "-o", "json")
		if err == nil {
			var js k8sJobStatus
			if jerr := json.Unmarshal(raw, &js); jerr == nil {
				for _, c := range js.Status.Conditions {
					if c.Status != "True" {
						continue
					}
					switch c.Type {
					case "Complete", "SuccessCriteriaMet":
						return nil
					case "Failed":
						return fmt.Errorf("%w: job/%s in %s: %s %s", ErrJobFailed, name, ns, c.Reason, strings.TrimSpace(c.Message))
					}
				}
			} else {
				lastErr = jerr
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for job/%s in %s to complete (last error: %v)", timeout, name, ns, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jobPollInterval):
		}
	}
}

// containerExit is one pod's trainer container outcome.
type containerExit struct {
	Pod      string
	ExitCode int
	Reason   string
	// PodFailed is set when the pod as a whole is Failed without the
	// container having recorded a terminated state (evicted, node lost,
	// OOM-killed before the status landed): no exit code, but not a success.
	PodFailed bool
}

// Failed reports whether this outcome is a trainer failure.
func (e containerExit) Failed() bool { return e.PodFailed || e.ExitCode != 0 }

// waitForContainerExit polls the pods matching selector until, in every
// one of them that runs the named container, it has terminated (and at
// least one such pod matched). The pod itself may still be running: that is
// the point, a keepalive sidecar holds it open so traces can be harvested
// after the trainer exits. A pod already Succeeded or Failed as a whole
// counts as terminated too; Failed without a container exit is reported as
// PodFailed. Pods whose container statuses name no such container (Ray
// workers under a selector shared with the trace harvest) are skipped; a
// pod with no statuses yet (Pending) still blocks.
func waitForContainerExit(ctx context.Context, exec CommandExecutor, env map[string]string, ns, selector, container string, timeout time.Duration) ([]containerExit, error) {
	if timeout <= 0 {
		timeout = jobWaitTimeout
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		raw, err := runStdout(ctx, exec, env, "-n", ns, "get", "pods", "-l", selector, "-o", "json")
		if err == nil {
			var pl k8sPodListStatus
			if jerr := json.Unmarshal(raw, &pl); jerr == nil && len(pl.Items) > 0 {
				var exits []containerExit
				allDone := true
				for _, p := range pl.Items {
					ce := containerExit{Pod: p.Metadata.Name}
					terminated, hasContainer := false, false
					for _, cs := range p.Status.ContainerStatuses {
						if cs.Name != container {
							continue
						}
						hasContainer = true
						if cs.State.Terminated != nil {
							terminated = true
							ce.ExitCode = cs.State.Terminated.ExitCode
							ce.Reason = cs.State.Terminated.Reason
						}
					}
					if !hasContainer && len(p.Status.ContainerStatuses) > 0 {
						continue // a pod of the selector that does not run the trainer
					}
					switch {
					case terminated:
					case p.Status.Phase == "Failed":
						ce.PodFailed, ce.Reason = true, "PodFailed"
					case p.Status.Phase == "Succeeded":
					default:
						allDone = false
					}
					if !allDone {
						break
					}
					exits = append(exits, ce)
				}
				if allDone && len(exits) > 0 {
					return exits, nil
				}
			} else if jerr != nil {
				lastErr = jerr
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for container %q of pods %q in %s to exit (last error: %v)", timeout, container, selector, ns, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(jobPollInterval):
		}
	}
}
