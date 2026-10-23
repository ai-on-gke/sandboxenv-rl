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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const goodDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func manifest(ids ...string) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(`{"instance_id":"` + id + `","repo":"r","image":"reg/img@` + goodDigest + `","patch":"p","eval_script":"e","log_parser":"parse_log_pytest"}` + "\n")
	}
	return b.String()
}

func TestParseDataset(t *testing.T) {
	got, err := ParseDataset(strings.NewReader("\n" + manifest("a__a-1", "b__b-2") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].InstanceID != "a__a-1" || got[1].InstanceID != "b__b-2" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseDatasetRejects(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"bad json":    "{not json}\n",
		"no id":       `{"image":"reg/img@` + goodDigest + `"}` + "\n",
		"no image":    `{"instance_id":"x"}` + "\n",
		"tag not pin": `{"instance_id":"x","image":"reg/img:latest"}` + "\n",
		"duplicate":   manifest("a", "a"),
	}
	for name, in := range cases {
		if _, err := ParseDataset(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSelect(t *testing.T) {
	all, err := ParseDataset(strings.NewReader(manifest("a", "b", "c")))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Select(all, nil, nil, 2)
	if err != nil || len(got) != 2 || got[1].InstanceID != "b" {
		t.Fatalf("limit: got %+v, %v", got, err)
	}
	got, err = Select(all, []string{"c", "a"}, nil, 0)
	if err != nil || len(got) != 2 || got[0].InstanceID != "a" || got[1].InstanceID != "c" {
		t.Fatalf("ids keep manifest order: got %+v, %v", got, err)
	}
	got, err = Select(all, []string{"c", "a"}, nil, 1)
	if err != nil || len(got) != 1 || got[0].InstanceID != "a" {
		t.Fatalf("ids then limit: got %+v, %v", got, err)
	}
	if _, err := Select(all, []string{"zz"}, nil, 0); err == nil {
		t.Fatal("unknown id must error")
	}
}

func TestSelectExcludeAndIDList(t *testing.T) {
	all, err := ParseDataset(strings.NewReader(manifest("a", "b", "c")))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Select(all, nil, []string{"b", "unknown"}, 0)
	if err != nil || len(got) != 2 || got[0].InstanceID != "a" || got[1].InstanceID != "c" {
		t.Fatalf("exclude: got %+v, %v", got, err)
	}
	if _, err := Select(all, []string{"b"}, []string{"b"}, 0); err == nil || !strings.Contains(err.Error(), "exclude list") {
		t.Fatalf("an excluded id asked for by name must say so: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ids.txt")
	if err := os.WriteFile(path, []byte("# known network-dependent\nb needs pypi\n\n  c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := ReadIDList(path)
	if err != nil || len(ids) != 2 || ids[0] != "b" || ids[1] != "c" {
		t.Fatalf("ReadIDList: %v, %v", ids, err)
	}
}

func TestLoadDatasetRefusesPlainHTTP(t *testing.T) {
	if _, err := LoadDataset("http://example.invalid/manifest.jsonl"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http must be refused: %v", err)
	}
}

// The https rule holds for each redirect hop, not only for the first URL.
func TestLoadDatasetRefusesARedirectToPlainHTTP(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		_, _ = w.Write([]byte(manifest("a")))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-http":
			http.Redirect(w, r, plain.URL+"/m.jsonl", http.StatusFound)
		case "/to-https":
			http.Redirect(w, r, "/m.jsonl", http.StatusFound)
		default:
			_, _ = w.Write([]byte(manifest("a")))
		}
	}))
	defer secure.Close()
	// LoadDataset uses the default transport; trust the test server's certificate.
	saved := http.DefaultTransport
	http.DefaultTransport = secure.Client().Transport
	defer func() { http.DefaultTransport = saved }()

	if _, err := LoadDataset(secure.URL + "/to-http"); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("redirect to http: err = %v, want refused", err)
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("the plain-http server was fetched %d times", n)
	}
	got, err := LoadDataset(secure.URL + "/to-https")
	if err != nil || len(got) != 1 {
		t.Fatalf("redirect to https: %v, %v; want the manifest", got, err)
	}
}

func TestRequireGrading(t *testing.T) {
	full, err := ParseDataset(strings.NewReader(manifest("a")))
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireGrading(full); err != nil {
		t.Fatalf("complete record: %v", err)
	}
	imageOnly, err := ParseDataset(strings.NewReader(`{"instance_id":"x","image":"reg/img@` + goodDigest + `"}` + "\n"))
	if err != nil {
		t.Fatalf("an image-only record must parse: %v", err)
	}
	if err := RequireGrading(imageOnly); err == nil || !strings.Contains(err.Error(), "eval_script") {
		t.Fatalf("image-only record must fail grading requirement: %v", err)
	}
	noParser, _ := ParseDataset(strings.NewReader(`{"instance_id":"x","image":"reg/img@` + goodDigest + `","eval_script":"e"}` + "\n"))
	if err := RequireGrading(noParser); err == nil || !strings.Contains(err.Error(), "log_parser") {
		t.Fatalf("missing log_parser must fail grading requirement: %v", err)
	}
}

type customValidateAdapter struct {
	kind       string
	valErr     error
	gradingErr error
}

func (a customValidateAdapter) Kind() string                          { return a.kind }
func (a customValidateAdapter) Validate(Instance) error               { return a.valErr }
func (a customValidateAdapter) RequireGrading(Instance) error         { return a.gradingErr }
func (a customValidateAdapter) TemplateData(in Instance) (any, error) { return in, nil }
func (a customValidateAdapter) Grades(*Trajectory) bool               { return false }
func (a customValidateAdapter) Grader() Grader                        { return nil }

func TestInstanceUnmarshalKindAndMaterial(t *testing.T) {
	// Default row omits "kind": Kind stays "", Material stays nil, and AdapterFor("") maps to DefaultKind.
	var def Instance
	if err := json.Unmarshal([]byte(`{"instance_id":"x","image":"reg/img@`+goodDigest+`"}`), &def); err != nil {
		t.Fatal(err)
	}
	if def.Kind != "" || def.Material != nil {
		t.Fatalf("default instance: Kind=%q Material=%s, want empty", def.Kind, string(def.Material))
	}
	if a := AdapterFor(def.Kind); a == nil || a.Kind() != DefaultKind {
		t.Fatalf("AdapterFor(%q) = %v, want %q", def.Kind, a, DefaultKind)
	}
	// Non-default kind without explicit "material" captures raw JSON in Material.
	rawRow := `{"instance_id":"x","image":"reg/img@` + goodDigest + `","kind":"custom-kind","extra":"val"}`
	var customImplicit Instance
	if err := json.Unmarshal([]byte(rawRow), &customImplicit); err != nil {
		t.Fatal(err)
	}
	if customImplicit.Kind != "custom-kind" || string(customImplicit.Material) != rawRow {
		t.Fatalf("implicit material: Kind=%q Material=%s", customImplicit.Kind, string(customImplicit.Material))
	}
	// Non-default kind with explicit "material" preserves only the sub-object.
	var customExplicit Instance
	if err := json.Unmarshal([]byte(`{"instance_id":"x","image":"reg/img@`+goodDigest+`","kind":"custom-kind","material":{"k":"v"}}`), &customExplicit); err != nil {
		t.Fatal(err)
	}
	if string(customExplicit.Material) != `{"k":"v"}` {
		t.Fatalf("explicit material = %s, want {\"k\":\"v\"}", string(customExplicit.Material))
	}
}

func TestValidateAndRequireGradingAdapterDispatch(t *testing.T) {
	// An unregistered kind fails both Validate (and ParseDataset) and RequireGrading.
	unknown := Instance{InstanceID: "u", Image: "reg/img@" + goodDigest, Kind: "unregistered-kind"}
	if err := unknown.Validate(); err == nil || !strings.Contains(err.Error(), "no dataset adapter registered") {
		t.Fatalf("Validate on unknown kind = %v, want unregistered error", err)
	}
	if _, err := ParseDataset(strings.NewReader(`{"instance_id":"u","image":"reg/img@` + goodDigest + `","kind":"unregistered-kind"}` + "\n")); err == nil || !strings.Contains(err.Error(), "no dataset adapter registered") {
		t.Fatalf("ParseDataset on unknown kind = %v, want unregistered error", err)
	}
	if err := RequireGrading([]Instance{unknown}); err == nil || !strings.Contains(err.Error(), "no dataset adapter registered") {
		t.Fatalf("RequireGrading on unknown kind = %v, want unregistered error", err)
	}

	// A registered adapter's Validate and RequireGrading errors are dispatched.
	RegisterAdapter(customValidateAdapter{
		kind:       "custom-val",
		valErr:     errors.New("custom validate failed"),
		gradingErr: errors.New("custom grading failed"),
	})
	t.Cleanup(func() { unregisterAdapter("custom-val") })

	in := Instance{InstanceID: "c", Image: "reg/img@" + goodDigest, Kind: "custom-val"}
	if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "custom validate failed") {
		t.Fatalf("Validate dispatch = %v, want custom validate failed", err)
	}
	if err := RequireGrading([]Instance{in}); err == nil || !strings.Contains(err.Error(), "custom grading failed") {
		t.Fatalf("RequireGrading dispatch = %v, want custom grading failed", err)
	}
}
