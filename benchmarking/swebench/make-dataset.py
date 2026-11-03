#!/usr/bin/env python3
#
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

"""Build the JSONL manifest swebench-loader reads from a SWE-bench dataset.

For every instance it records the gold patch, the test patch, the harness
eval script (the exact script the SWE-bench harness runs: apply the test
patch, run the repo's test command, restore the test files, print the exit
code marker the grader reads) and the task image pinned by digest in the
registry the cluster can pull from.

The eval script comes from the installed swebench's make_test_spec, which
wraps the dataset row's own eval_script with the harness header and adds the
two lines that record the test command's exit status ("Test Exit Code"); the
two scripts are checked to agree line for line apart from those additions.
The dataset is loaded at a resolved commit (the --revision given, else the
current head), and that commit, the swebench version and the file hash of a
local dataset are recorded in OUT.meta.json, so two operators build the same
manifest.

    pip install datasets 'swebench>=5'
    ./make-dataset.py --registry us-central1-docker.pkg.dev/PROJECT/swebench \
        --out swebench-verified.jsonl

The image reference per instance comes from --image-ref, a format string
over {registry}, {instance_id} and {instance_id_1776}. The default matches a
mirror that keeps one tag per instance in a single repository; the public
SWE-bench images on Docker Hub are one repository per instance:

    --image-ref '{registry}/sweb.eval.x86_64.{instance_id_1776}:latest'

Instances whose image cannot be resolved are reported and left out, so run
mirror-images.sh first and re-run this until none are missing.

For image-delivery workloads that grade nothing (workload-file-read,
workload-compile, workload-read-all), build an image-only manifest from a
list of references instead:

    ./make-dataset.py --images-file images.txt --out images.jsonl
"""

import argparse
import datetime
import hashlib
import json
import hashlib
import subprocess
import sys

# The lines swebench >= 5 adds around the end marker to record the test
# command's own exit status (swebench.harness.utils.record_test_exit_code).
# They are not in the dataset row's eval_script.
_EXIT_CODE_VAR = "SWEBENCH_TEST_EXIT_CODE"
_EXIT_CODE_MARKER = ">>>>> Test Exit Code"
_HEADER = ("#!/bin/bash", "set -uxo pipefail")


def _exit_code_names() -> tuple[str, str]:
    try:
        from swebench.harness.constants import TEST_EXIT_CODE, TEST_EXIT_CODE_VAR
        return TEST_EXIT_CODE_VAR, TEST_EXIT_CODE
    except ImportError:
        return _EXIT_CODE_VAR, _EXIT_CODE_MARKER


def script_lines(script: str, *, harness: bool = False) -> list[str]:
    """The command lines of an eval script, without the shell header.

    With harness=True the exit-code lines the harness adds are dropped too.
    """
    var, marker = _exit_code_names()
    lines = []
    for line in script.strip().splitlines():
        if line in _HEADER:
            continue
        if harness and (line == f"{var}=$?" or line.startswith(f'echo "{marker}:')):
            continue
        lines.append(line)
    return lines


def row_script_matches(row_script: str, harness_script: str) -> bool:
    """Whether the harness runs the dataset row's eval script, apart from the
    header and the exit-code lines it adds."""
    return script_lines(harness_script, harness=True) == script_lines(row_script)


def resolve_revision(dataset: str, revision: str | None) -> str | None:
    """The dataset's commit (Hugging Face), or the sha256 of a local file."""
    if dataset.endswith((".json", ".jsonl")):
        with open(dataset, "rb") as f:
            return "sha256:" + hashlib.sha256(f.read()).hexdigest()
    try:
        from huggingface_hub import HfApi
    except ImportError:
        return None
    return HfApi().dataset_info(dataset, revision=revision).sha


def image_ref(registry: str, instance_id: str, fmt: str) -> str:
    return fmt.format(
        registry=registry,
        instance_id=instance_id,
        instance_id_1776=instance_id.replace("__", "_1776_"),
    )


def split_ref(ref: str) -> tuple[str, str, str]:
    """Split an image reference into (repository, tag, digest); tag and digest
    are empty when absent. The tag is only in the last path segment, so a
    registry port such as host:5000/img is left alone."""
    repo, _, digest = ref.partition("@")
    head, _, last = repo.rpartition("/")
    tag = ""
    if ":" in last:
        last, tag = last.split(":", 1)
    return (f"{head}/{last}" if head else last), tag, digest


def resolve_digest(ref: str) -> str | None:
    """Return repo@sha256:... for ref, or None when crane cannot see it."""
    proc = subprocess.run(["crane", "digest", ref], capture_output=True, text=True)
    if proc.returncode != 0:
        return None
    repo, _, _ = split_ref(ref)
    return f"{repo}@{proc.stdout.strip()}"


def pin_ref(ref: str) -> str | None:
    """repo@sha256:... for ref: a reference that already carries a digest is
    reduced to it (a tag next to the digest is dropped, ParseDataset accepts
    only repo@digest); a tag is resolved with crane."""
    repo, _, digest = split_ref(ref)
    if digest.startswith("sha256:"):
        return f"{repo}@{digest}"
    return resolve_digest(ref)


def instance_id_from_ref(ref: str, pinned: str) -> str:
    """A stable id for an image-only record: the last path segment, the tag if
    one was given, and the first 12 hex digits of the digest, so two digest-
    pinned images of one repository get distinct ids. Characters outside
    [a-z0-9._-] are replaced so the id survives EnvID."""
    repo, tag, _ = split_ref(ref)
    _, _, digest = split_ref(pinned)
    last = repo.rsplit("/", 1)[-1]
    short = digest.split(":", 1)[1][:12]
    raw = f"{last}-{tag}-{short}" if tag and tag != "latest" else f"{last}-{short}"
    return "".join(c if c.isalnum() or c in "._-" else "-" for c in raw.lower())


def write_image_only(images_file: str, out: str, limit: int = 0) -> int:
    """Manifest for image-delivery workloads: one record per image reference,
    no grading material. References may be tags (resolved to a digest with
    crane) or already pinned by digest. A reference whose digest an earlier
    line already carries is skipped, whatever its repository: ate-env-api
    derives one template per digest and refuses a second reference to it, so
    the duplicate could only fail at create time. Two different images whose
    ids would still collide get a hash of the full reference appended.
    limit > 0 stops after that many records."""
    written = missing = 0
    digest_seen: dict[str, str] = {}
    ids: set[str] = set()
    with open(images_file) as src, open(out, "w") as dst:
        for line in src:
            ref = line.strip()
            if not ref or ref.startswith("#"):
                continue
            if limit and written >= limit:
                break
            pinned = pin_ref(ref)
            if pinned is None:
                print(f"missing image: {ref}", file=sys.stderr)
                missing += 1
                continue
            digest = pinned.rsplit("@", 1)[1]
            if digest in digest_seen:
                print(f"duplicate image: {ref} has the digest of {digest_seen[digest]}, already written", file=sys.stderr)
                continue
            digest_seen[digest] = pinned
            iid = instance_id_from_ref(ref, pinned)
            if iid in ids:
                iid = f"{iid}-{hashlib.sha256(pinned.encode()).hexdigest()[:8]}"
            ids.add(iid)
            dst.write(json.dumps({"instance_id": iid, "repo": "", "image": pinned,
                                  "patch": "", "test_patch": "", "eval_script": "",
                                  "fail_to_pass": [], "pass_to_pass": [], "log_parser": "", "eval_type": ""}) + "\n")
            written += 1
    with open(out + ".meta.json", "w") as f:
        json.dump({"source": images_file, "instances": written, "missing_images": missing, "grading": False,
                   "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")}, f, indent=2)
        f.write("\n")
    print(f"wrote {written} image-only instances to {out}; {missing} missing images", file=sys.stderr)
    return 1 if missing else 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dataset", default="SWE-bench/SWE-bench_Verified", help="Hugging Face dataset name or a local JSON/JSONL path. The rows must carry eval_script (the SWE-bench org copies do; the older princeton-nlp copies do not).")
    ap.add_argument("--split", default="test")
    ap.add_argument("--revision", default=None, help="Dataset revision (commit, tag or branch) to load; default the current head. The resolved commit is what is loaded and recorded in the meta file.")
    ap.add_argument("--images-file", default=None, help="Build an image-only manifest (no grading material) from a file with one image reference per line, for the image-delivery workloads. --limit applies; --dataset, --registry and --instances are ignored.")
    ap.add_argument("--registry", default=None, help="Registry path that holds the task images, e.g. us-docker.pkg.dev/PROJECT/swebench-mirror/swebench-verified. Required unless --images-file is given.")
    ap.add_argument("--image-ref", default="{registry}:sweb.eval.x86_64.{instance_id}", help="Image reference per instance; fields: {registry}, {instance_id}, {instance_id_1776}.")
    ap.add_argument("--limit", type=int, default=0, help="Stop after N instances (0 = all).")
    ap.add_argument("--instances", default="", help="Comma-separated instance ids to include; default all.")
    ap.add_argument("--out", required=True, help="JSONL manifest to write.")
    args = ap.parse_args()

    if args.images_file:
        return write_image_only(args.images_file, args.out, args.limit)
    if not args.registry:
        ap.error("--registry is required unless --images-file is given")

    try:
        from datasets import load_dataset
    except ImportError as e:
        print(f"missing dependency: {e}; pip install datasets", file=sys.stderr)
        return 2
    try:
        make_test_spec = _load_make_test_spec()
    except ImportError as e:
        print(f"cannot import make_test_spec from swebench: {e}; pip install -U 'swebench>=5'", file=sys.stderr)
        return 2

    try:
        resolved = resolve_revision(args.dataset, args.revision)
    except Exception as e:  # network, auth, unknown revision
        print(f"cannot resolve the revision of {args.dataset}: {e}", file=sys.stderr)
        return 2
    if resolved is None:
        print("cannot resolve the dataset revision: huggingface_hub is missing; pip install datasets", file=sys.stderr)
        return 2
    if args.dataset.endswith((".json", ".jsonl")):
        rows = load_dataset("json", data_files=args.dataset, split="train")
    else:
        # Load the commit that is recorded, not whatever the head is by then.
        rows = load_dataset(args.dataset, split=args.split, revision=resolved)
    import swebench
    swebench_version = getattr(swebench, "__version__", "unknown")
    want = {s for s in args.instances.split(",") if s}

    written = missing = 0
    with open(args.out, "w") as out:
        for row in rows:
            iid = row["instance_id"]
            if want and iid not in want:
                continue
            if args.limit and written >= args.limit:
                break
            ref = image_ref(args.registry, iid, args.image_ref)
            pinned = resolve_digest(ref)
            if pinned is None:
                print(f"missing image for {iid}: {ref}", file=sys.stderr)
                missing += 1
                continue
            try:
                spec = make_test_spec(row)
            except KeyError as e:
                print(f"{iid}: dataset row lacks field {e} that this swebench release needs; "
                      "use a dataset revision that carries eval_script (swebench >= 5) or an older swebench", file=sys.stderr)
                return 2
            if row.get("eval_script") and not row_script_matches(row["eval_script"], spec.eval_script):
                print(f"{iid}: the installed swebench's eval script differs from the dataset row's "
                      "beyond the header and the exit-code lines; check the swebench version against "
                      "the dataset revision", file=sys.stderr)
                return 2
            rec = {
                "instance_id": iid,
                "repo": row["repo"],
                "image": pinned,
                "patch": row["patch"],
                "test_patch": row["test_patch"],
                "eval_script": spec.eval_script,
                "fail_to_pass": _as_list(row["FAIL_TO_PASS"]),
                "pass_to_pass": _as_list(row["PASS_TO_PASS"]),
                "log_parser": row["log_parser"],
                "eval_type": row["eval_type"],
            }
            out.write(json.dumps(rec) + "\n")
            written += 1
    meta = {
        "dataset": args.dataset,
        "split": args.split,
        "revision": args.revision,
        "resolved_revision": resolved,
        "swebench_version": swebench_version,
        "registry": args.registry,
        "image_ref": args.image_ref,
        "instances": written,
        "missing_images": missing,
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
    }
    with open(args.out + ".meta.json", "w") as f:
        json.dump(meta, f, indent=2)
        f.write("\n")
    print(f"wrote {written} instances to {args.out}; {missing} missing images; provenance in {args.out}.meta.json", file=sys.stderr)
    return 1 if missing else 0


def _load_make_test_spec():
    """make_test_spec moved between swebench releases; try the known homes."""
    import importlib

    import swebench

    errors = []
    for mod in ("swebench.harness.utils", "swebench.harness.test_spec.test_spec", "swebench.harness.test_spec"):
        try:
            return getattr(importlib.import_module(mod), "make_test_spec")
        except (ImportError, AttributeError) as e:
            errors.append(f"{mod}: {e}")
    version = getattr(swebench, "__version__", "unknown")
    raise ImportError(f"swebench {version} at {swebench.__file__}: " + "; ".join(errors))


def _as_list(v):
    if isinstance(v, str):
        return json.loads(v)
    return list(v)


if __name__ == "__main__":
    sys.exit(main())
