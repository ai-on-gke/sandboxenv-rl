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
	"strings"
	"testing"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
	"pgregory.net/rapid"
)

var hexDigit = rapid.SampledFrom([]rune("0123456789abcdef"))

func genDigest() *rapid.Generator[string] {
	return rapid.StringOfN(hexDigit, 64, 64, -1)
}

func genRepo() *rapid.Generator[string] {
	seg := rapid.StringMatching(`[a-z0-9]([a-z0-9._-]{0,6}[a-z0-9])?`)
	return rapid.Custom(func(t *rapid.T) string {
		host := rapid.SampledFrom([]string{"docker.io", "example.com", "gcr.io", "localhost:5000", "us-docker.pkg.dev"}).Draw(t, "host")
		n := rapid.IntRange(1, 4).Draw(t, "segs")
		parts := []string{host}
		for i := range n {
			parts = append(parts, seg.Draw(t, "seg"+string(rune('0'+i))))
		}
		ref := strings.Join(parts, "/")
		if rapid.Bool().Draw(t, "tag") {
			ref += ":" + rapid.StringMatching(`[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,8}`).Draw(t, "tagv")
		}
		return ref
	})
}

func genShortName(maxLen int) *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		n := rapid.IntRange(1, maxLen).Draw(t, "len")
		if n == 1 {
			return rapid.StringOfN(rapid.SampledFrom([]rune("abcz09")), 1, 1, -1).Draw(t, "c")
		}
		first := rapid.StringOfN(rapid.SampledFrom([]rune("abcz09")), 1, 1, -1).Draw(t, "first")
		mid := rapid.StringOfN(rapid.SampledFrom([]rune("abz09-")), n-2, n-2, -1).Draw(t, "mid")
		last := rapid.StringOfN(rapid.SampledFrom([]rune("abz09")), 1, 1, -1).Draw(t, "last")
		return first + mid + last
	})
}

// AE-3 (docs/plans/ate-env-api/acceptance-criteria.md). Rule (ImageTemplateName doc, CreateEnvironment doc, AGENTS.md): the derived
// name is "<base>-<first 12 hex of the digest>": a function of base and digest
// only (repository path and tag do not matter), and for a valid base short
// enough it is a valid k8s short name; distinct (base, digest12) never
// collide.
func TestPropImageTemplateNameIdentity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base := genShortName(50).Draw(t, "base")
		d := genDigest().Draw(t, "digest")
		r1, r2 := genRepo().Draw(t, "repo1"), genRepo().Draw(t, "repo2")
		n1, err1 := ImageTemplateName(base, r1+"@sha256:"+d)
		n2, err2 := ImageTemplateName(base, r2+"@sha256:"+d)
		if err1 != nil || err2 != nil {
			t.Fatalf("valid inputs rejected: %v / %v", err1, err2)
		}
		if n1 != n2 {
			t.Fatalf("same digest, different names: %q vs %q", n1, n2)
		}
		if n1 != base+"-"+d[:12] || len(n1) > 63 || !shortName.MatchString(n1) {
			t.Fatalf("bad derived name %q", n1)
		}
		// Injectivity over (base, digest12).
		base2 := genShortName(50).Draw(t, "base2")
		d2 := genDigest().Draw(t, "digest2")
		m, err := ImageTemplateName(base2, r1+"@sha256:"+d2)
		if err != nil {
			t.Fatal(err)
		}
		if (m == n1) != (base2 == base && d2[:12] == d[:12]) {
			t.Fatalf("collision: (%q,%q)->%q and (%q,%q)->%q", base, d, n1, base2, d2, m)
		}
	})
}

// AE-3. Rule: ImageTemplateName never panics and never returns an invalid name,
// whatever the input.
func TestPropImageTemplateNameNeverInvalid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base := rapid.String().Draw(t, "base")
		image := rapid.OneOf(rapid.String(), rapid.Custom(func(t *rapid.T) string {
			return rapid.String().Draw(t, "p") + "@sha256:" + genDigest().Draw(t, "d")
		})).Draw(t, "image")
		name, err := ImageTemplateName(base, image)
		if err == nil && (len(name) > 63 || !shortName.MatchString(name)) {
			t.Fatalf("invalid name %q accepted", name)
		}
	})
}

// AE-2 (docs/plans/ate-env-api/acceptance-criteria.md). Rule (rerootCommand doc): a guest command that pointed into the guest
// image's root moves under GuestMountPath; one that cannot is an error, never
// "a template whose guest is silently not found". Thus every accepted
// re-rooted command[0] names, under GuestMountPath, the same file command[0]
// named in the guest image's root.
func TestPropRerootStaysUnderMount(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		segs := rapid.SliceOfN(rapid.SampledFrom([]string{"ko-app", "usr", "bin", "..", ".", "", "x"}), 0, 5).Draw(t, "segs")
		cmd0 := "/" + strings.Join(append(segs, guestBinaryName), "/")
		out, err := rerootCommand([]string{cmd0, "-flag"})
		if err != nil {
			return
		}
		// Resolve as the kernel would: in the guest image's root, ".." at
		// "/" stays at "/".
		want := GuestMountPath + cleanRooted(cmd0)
		if out[0] != want {
			t.Fatalf("rerootCommand(%q)[0] = %q, want %q (the same file under the mount)", cmd0, out[0], want)
		}
	})
}

// cleanRooted resolves a rooted path lexically, as a lookup from "/" does.
func cleanRooted(p string) string {
	var stack []string
	for _, s := range strings.Split(p, "/") {
		switch s {
		case "", ".":
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			stack = append(stack, s)
		}
	}
	return "/" + strings.Join(stack, "/")
}

// The shrunk counterexample of TestPropRerootStaysUnderMount, pinned.
func TestRerootKeepsDotDotUnderMount(t *testing.T) {
	out, err := rerootCommand([]string{"/../ko-app/ate-env-guest"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out[0] != GuestMountPath+"/ko-app/ate-env-guest" {
		t.Errorf("rerootCommand(/../ko-app/ate-env-guest) = %q: outside %s, the guest is looked up in the rootfs image", out[0], GuestMountPath)
	}
}

// AE-4 (docs/plans/ate-env-api/acceptance-criteria.md). Rule (DeriveImageTemplate doc): deriving from an already-derived template
// changes only the container image (and the name); thus
// Derive(Derive(base, n1, i1), n2, i2) == Derive(base, n2, i2).
func TestPropDeriveComposes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base := modeABase()
		nLayers := rapid.IntRange(0, 2).Draw(t, "layers")
		for i := range nLayers {
			name := rapid.SampledFrom([]string{"execd", "py", "tools"}).Draw(t, "layer") + string(rune('a'+i))
			base.Volumes = append(base.Volumes, &ateapipb.Volume{Name: name, Image: &ateapipb.ImageVolumeSource{Reference: "example.com/l@sha256:" + genDigest().Draw(t, "ld")}})
			base.Containers[0].VolumeMounts = append(base.Containers[0].VolumeMounts, &ateapipb.VolumeMount{Name: name, MountPath: "/opt/" + name})
		}
		if rapid.Bool().Draw(t, "nocmd") {
			base.Containers[0].Command = nil
		}
		i1 := genRepo().Draw(t, "r1") + "@sha256:" + genDigest().Draw(t, "d1")
		i2 := genRepo().Draw(t, "r2") + "@sha256:" + genDigest().Draw(t, "d2")
		before := proto.Clone(base)
		d1, err := DeriveImageTemplate(base, "n1", i1)
		if err != nil {
			t.Fatal(err)
		}
		twice, err := DeriveImageTemplate(d1, "n2", i2)
		if err != nil {
			t.Fatal(err)
		}
		once, err := DeriveImageTemplate(base, "n2", i2)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(base, before) {
			t.Fatal("base mutated")
		}
		if !proto.Equal(twice, once) {
			t.Fatalf("derive does not compose:\n twice=%v\n once=%v", twice, once)
		}
		// Volume names stay unique.
		seen := map[string]bool{}
		for _, v := range once.GetVolumes() {
			if seen[v.GetName()] {
				t.Fatalf("duplicate volume %q", v.GetName())
			}
			seen[v.GetName()] = true
		}
	})
}

// AE-1 (supporting). Rule (sameImageDigest doc): two pinned references name the same image iff
// their digests are equal, whatever the repository path.
func TestPropSameImageDigest(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d1, d2 := genDigest().Draw(t, "d1"), genDigest().Draw(t, "d2")
		a := genRepo().Draw(t, "a") + "@sha256:" + d1
		b := genRepo().Draw(t, "b") + "@sha256:" + d2
		if sameImageDigest(a, b) != (d1 == d2) || !sameImageDigest(a, a) || sameImageDigest(a, b) != sameImageDigest(b, a) {
			t.Fatalf("sameImageDigest(%q,%q) wrong", a, b)
		}
	})
}
