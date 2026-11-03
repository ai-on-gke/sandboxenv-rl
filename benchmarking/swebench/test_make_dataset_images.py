# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""make-dataset.py's image-only manifest: reference parsing, ids and limits,
without crane or a network."""

from __future__ import annotations

import importlib.util
import json
import pathlib

_SPEC = importlib.util.spec_from_file_location(
    "make_dataset", pathlib.Path(__file__).with_name("make-dataset.py"))
md = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(md)

D1 = "sha256:" + "a" * 64
D2 = "sha256:" + "b" * 64


def test_split_ref():
    assert md.split_ref("reg.io/p/img:tag@" + D1) == ("reg.io/p/img", "tag", D1)
    assert md.split_ref("reg.io/p/img@" + D1) == ("reg.io/p/img", "", D1)
    assert md.split_ref("reg.io/p/img:tag") == ("reg.io/p/img", "tag", "")
    assert md.split_ref("host:5000/img") == ("host:5000/img", "", "")
    assert md.split_ref("img") == ("img", "", "")


def test_pin_ref_drops_tag_next_to_digest(monkeypatch):
    monkeypatch.setattr(md, "resolve_digest", lambda ref: (_ for _ in ()).throw(AssertionError(ref)))
    assert md.pin_ref("reg.io/p/img:tag@" + D1) == "reg.io/p/img@" + D1
    assert md.pin_ref("reg.io/p/img@" + D1) == "reg.io/p/img@" + D1


def test_pin_ref_resolves_tags(monkeypatch):
    monkeypatch.setattr(md, "resolve_digest", lambda ref: "reg.io/p/img@" + D2 if ref == "reg.io/p/img:tag" else None)
    assert md.pin_ref("reg.io/p/img:tag") == "reg.io/p/img@" + D2
    assert md.pin_ref("reg.io/p/other:tag") is None


def test_instance_id_distinct_per_digest():
    a = md.instance_id_from_ref("reg.io/p/img@" + D1, "reg.io/p/img@" + D1)
    b = md.instance_id_from_ref("reg.io/p/img@" + D2, "reg.io/p/img@" + D2)
    assert a == "img-" + "a" * 12
    assert b == "img-" + "b" * 12
    assert md.instance_id_from_ref("reg.io/p/Img:V1.2_x", "reg.io/p/Img@" + D1) == "img-v1.2_x-" + "a" * 12
    assert md.instance_id_from_ref("reg.io/p/img:latest", "reg.io/p/img@" + D1) == "img-" + "a" * 12


def _write(tmp_path, lines, monkeypatch, limit=0):
    monkeypatch.setattr(md, "resolve_digest", lambda ref: None)
    src = tmp_path / "images.txt"
    src.write_text("".join(l + "\n" for l in lines))
    out = tmp_path / "images.jsonl"
    rc = md.write_image_only(str(src), str(out), limit)
    rows = [json.loads(l) for l in out.read_text().splitlines()]
    meta = json.loads((out.with_name("images.jsonl.meta.json")).read_text())
    return rc, rows, meta


def test_write_image_only_unique_ids_and_dedupe(tmp_path, monkeypatch):
    rc, rows, meta = _write(tmp_path, [
        "# comment",
        "reg.io/a/img@" + D1,
        "reg.io/b/img@" + D1,        # same digest, other repository: the API keys templates by digest, skipped
        "reg.io/a/img@" + D2,        # same repository, other digest: distinct id
        "reg.io/a/img:tag@" + D2,    # same reference as the line above once the tag is dropped: skipped
        "reg.io/a/missing:tag",      # crane cannot see it
    ], monkeypatch)
    assert rc == 1
    ids = [r["instance_id"] for r in rows]
    assert len(ids) == len(set(ids)) == 2
    assert ids[0] == "img-" + "a" * 12 and rows[0]["image"] == "reg.io/a/img@" + D1
    assert ids[1] == "img-" + "b" * 12 and rows[1]["image"] == "reg.io/a/img@" + D2
    assert all(r["image"].count(":") == 1 and "@sha256:" in r["image"] for r in rows)
    assert all(r["eval_script"] == "" for r in rows)
    assert meta["instances"] == 2 and meta["missing_images"] == 1 and meta["grading"] is False


def test_write_image_only_id_collision_gets_hash(tmp_path, monkeypatch):
    # Two different pinned references with the same name, tag and digest prefix
    # cannot share an id, or ParseDataset rejects the manifest.
    rc, rows, _ = _write(tmp_path, ["reg.io/a/img@" + D1, "reg.io/a/img:latest@" + D1], monkeypatch)
    assert rc == 0 and len(rows) == 1  # same image: skipped, not collided
    short_a = "sha256:" + "a" * 12 + "c" * 52
    rc, rows, _ = _write(tmp_path, ["reg.io/a/img@" + D1, "reg.io/a/img@" + short_a], monkeypatch)
    assert rc == 0 and len(rows) == 2
    assert rows[0]["instance_id"] == "img-" + "a" * 12
    assert rows[1]["instance_id"].startswith("img-" + "a" * 12 + "-") and len(rows[1]["instance_id"]) == 16 + 1 + 8


def test_write_image_only_limit(tmp_path, monkeypatch):
    D3 = "sha256:" + "c" * 64
    rc, rows, meta = _write(tmp_path, ["reg.io/a/img@" + D1, "reg.io/a/img@" + D2, "reg.io/a/other@" + D3], monkeypatch, limit=2)
    assert rc == 0 and len(rows) == 2 and meta["instances"] == 2
