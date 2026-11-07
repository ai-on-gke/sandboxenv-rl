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

package schema

import (
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func TestJobSpecHeadJobAndPodSelectorDefaults(t *testing.T) {
	j := JobSpec{Name: "job-2", Kind: "real", RealMode: "measure"}
	if got := j.EffectiveHeadJob(); got != "job-2" {
		t.Fatalf("EffectiveHeadJob = %q, want name", got)
	}
	if got := j.EffectivePodSelector(); got != "job-name=job-2" {
		t.Fatalf("EffectivePodSelector = %q", got)
	}
	j.HeadJob = " trainer-2-head "
	j.PodSelector = "ray.io/cluster=trainer-2"
	if got := j.EffectiveHeadJob(); got != "trainer-2-head" {
		t.Fatalf("EffectiveHeadJob = %q, want trimmed override", got)
	}
	if got := j.EffectivePodSelector(); got != "ray.io/cluster=trainer-2" {
		t.Fatalf("EffectivePodSelector = %q, want override", got)
	}
}

func TestJobSpecWaitTimeout(t *testing.T) {
	for _, tc := range []struct {
		timeout string
		want    time.Duration
		wantErr string
	}{
		{"", time.Hour, ""},
		{"3h", 3 * time.Hour, ""},
		{" 90m ", 90 * time.Minute, ""},
		{"x", 0, "not a duration"},
		{"0", 0, "must be > 0"},
		{"-5m", 0, "must be > 0"},
	} {
		j := JobSpec{Name: "j", Kind: "real", RealMode: "measure", TrainerManifest: "m.yaml", Timeout: tc.timeout}
		d, err := j.WaitTimeout()
		if tc.wantErr == "" {
			if err != nil || d != tc.want {
				t.Fatalf("WaitTimeout(%q) = %v, %v; want %v", tc.timeout, d, err, tc.want)
			}
			if verr := j.Validate(); verr != nil {
				t.Fatalf("Validate with timeout %q: %v", tc.timeout, verr)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("WaitTimeout(%q) err = %v, want %q", tc.timeout, err, tc.wantErr)
		}
		if verr := j.Validate(); verr == nil || !strings.Contains(verr.Error(), tc.wantErr) {
			t.Fatalf("Validate with timeout %q = %v, want the same rejection", tc.timeout, verr)
		}
	}
}

func TestJobSpecTraceCollectionValidation(t *testing.T) {
	base := JobSpec{Name: "j", Kind: "real", RealMode: "trace", TrainerManifest: "m.yaml"}
	if err := base.Validate(); err != nil {
		t.Fatalf("base: %v", err)
	}
	for _, tc := range []struct {
		name    string
		tc      TraceCollection
		wantErr string
	}{
		{"ok", TraceCollection{RemoteDir: "/tmp/ate-traces"}, ""},
		{"missing dir", TraceCollection{PodSelector: "a=b"}, "remote_dir is required"},
		{"relative dir", TraceCollection{RemoteDir: "traces"}, "absolute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := base
			cfg := tc.tc
			j.TraceCollection = &cfg
			err := j.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestScenarioSchemaAcceptsRealJobFields(t *testing.T) {
	raw := []byte(`
schema_version: v1
name: multi-job
sut:
  backend: substrate
  access: ate-env-api
  endpoint: ate-env-api.default.svc.cluster.local:8080
jobs:
  - name: job-0
    kind: real
    real_mode: trace
    trainer_manifest: manifests/job-0.yaml
    head_job: trainer-0-head
    pod_selector: ray.io/cluster=trainer-0
    env_vars:
      TOTAL_STEPS: "40"
    trace_collection:
      pod_selector: ray.io/cluster=trainer-0
      container: trainer
      remote_dir: /tmp/ate-traces
`)
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgainstJSONSchema("scenario.schema.json", jsonBytes); err != nil {
		t.Fatalf("schema validate: %v", err)
	}
	sc, err := DecodeYAMLStrict[Scenario](raw)
	if err != nil {
		t.Fatalf("DecodeYAMLStrict: %v", err)
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("Scenario.Validate: %v", err)
	}
	j := sc.Jobs[0]
	if j.EffectiveHeadJob() != "trainer-0-head" || j.TraceCollection == nil || j.TraceCollection.RemoteDir != "/tmp/ate-traces" {
		t.Fatalf("unexpected decoded job: %+v", j)
	}

	// A relative remote_dir is rejected by the JSON schema too.
	bad := strings.Replace(string(raw), "remote_dir: /tmp/ate-traces", "remote_dir: tmp/ate-traces", 1)
	badJSON, _ := yaml.YAMLToJSON([]byte(bad))
	if err := ValidateAgainstJSONSchema("scenario.schema.json", badJSON); err == nil {
		t.Fatal("expected schema rejection of relative remote_dir")
	}
}
