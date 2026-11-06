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

"""Build a CommandAdapter JSONL manifest for swebench-loader from an R2E-Gym dataset.

Each row carries kind="command", a digest-pinned image reference, setup_command,
reset_command, and command so swebench-loader can replay and grade R2E-Gym tasks
without uni-agent or network access inside the sandbox.

Two-step workflow with Artifact Registry proxy or mirror:

    ./make-r2e-dataset.py --source r2e_gym_easy --images-out r2e-images.txt
    ../../integrations/uni-agent/scripts/resolve_images.sh \
        r2e-images.txt REGION-docker.pkg.dev/PROJECT/dockerhub-remote r2e-images.json
    ./make-r2e-dataset.py --source r2e_gym_easy --limit 20 \
        --image-digests r2e-images.json --out r2e-20.jsonl

Step one lists every candidate image, not the first --limit: step two skips a
tag whose digest an earlier tag already has, so it reads past the first --limit
candidates, and every tag it reads must be in --image-digests. The resolved
mapping serves any later --limit.
"""

from __future__ import annotations

import argparse
import base64
import datetime
import importlib.util
import json
import posixpath
import shlex
import sys
from pathlib import Path
from typing import Any

_MD_SPEC = importlib.util.spec_from_file_location(
    "make_dataset", Path(__file__).with_name("make-dataset.py")
)
assert _MD_SPEC is not None and _MD_SPEC.loader is not None
_md = importlib.util.module_from_spec(_MD_SPEC)
_MD_SPEC.loader.exec_module(_md)

pin_ref = _md.pin_ref
resolve_revision = _md.resolve_revision

R2E_EASY_DATASET = "hfilaretov/Benchmark-R2E-Gym-Easy"
R2E_EASY_FILES = {
    "train": "benchmark_r2e_gym_easy_train.jsonl",
    "val": "benchmark_r2e_gym_easy_val.jsonl",
}
R2E_SUBSET_DATASET = "R2E-Gym/R2E-Gym-Subset"

_INLINE_GRADER = """\
import base64, json, pathlib, re, sys
ANSI = re.compile(r"\\x1b\\[[0-9;]*m|\\[\\d+m")
MARKER = "short test summary info"
log = pathlib.Path(sys.argv[1]).read_text(errors="replace")
try:
    is_path = pathlib.Path(sys.argv[2]).is_file()
except OSError:
    is_path = False
exp_raw = json.loads(
    pathlib.Path(sys.argv[2]).read_text(encoding="utf-8")
    if is_path
    else base64.b64decode(sys.argv[2]).decode("utf-8")
)
norm = lambda m: {ANSI.sub("", str(k)).split(" - ", 1)[0].strip(): str(v) for k, v in (m or {}).items()}
expected = norm(exp_raw)
expected.pop("", None)
obs = {}
if MARKER in log:
    for raw in log.split(MARKER, 1)[1].splitlines():
        line = ANSI.sub("", raw).strip()
        for st in ("PASSED", "FAILED", "ERROR"):
            if line.startswith(st + " "):
                parts = line.split("::")
                tid = (".".join(parts[1:]) if len(parts) > 1 else line).split(" - ", 1)[0].strip()
                obs[tid] = st
                break
obs = norm(obs)
obs.pop("", None)
ok = bool(expected) and obs.keys() == expected.keys() and all(obs[k] == expected[k] for k in expected)
detail = f"matched {len(expected)}/{len(expected)} tests" if ok else f"observed {len(obs)} vs expected {len(expected)} tests"
print(json.dumps({"valid": True, "reward": 1 if ok else 0, "detail": detail}))
"""


def _heredoc_b64_write(data: bytes, delimiter: str, quoted_dest: str) -> str:
    """Decode base64 into quoted_dest via stdin heredoc so no argv scales with payload size."""
    encoded = base64.encodebytes(data).decode("ascii") if data else ""
    return f"base64 -d > {quoted_dest} <<'{delimiter}'\n{encoded}{delimiter}"


def _looks_like_test(path: str) -> bool:
    parts = path.split("/")
    return (
        path.endswith("_test.py")
        or parts[-1].startswith("test_")
        or any(p in ("test", "tests", "Test", "Tests") for p in parts[:-1])
    )


def _range(rng: dict[str, Any] | None) -> str:
    rng = rng or {}
    start = rng.get("start", 0)
    return f"{start},{rng['length']}" if rng.get("length") is not None else str(start)


def extract_gold_patch(
    parsed_commit_content: str | dict[str, Any], *, source_only: bool = True
) -> str:
    """Rebuild a unified diff of the gold commit from R2E-Gym parsed_commit_content."""
    if not parsed_commit_content:
        return ""
    data = (
        json.loads(parsed_commit_content)
        if isinstance(parsed_commit_content, str)
        else parsed_commit_content
    )
    chunks: list[str] = []
    for file_diff in data.get("file_diffs", []):
        header = file_diff.get("header") or {}
        path = ((header.get("file") or {}).get("path")) or ""
        if not path or (source_only and (not path.endswith(".py") or _looks_like_test(path))):
            continue
        chunks.append(f"diff --git a/{path} b/{path}\n")
        if header.get("misc_line"):
            chunks.append(header["misc_line"] + "\n")
        index_line = file_diff.get("index_line")
        if index_line:
            mode = index_line.get("mode") or ""
            chunks.append(
                f"index {index_line.get('old_commit_hash', '')}..{index_line.get('new_commit_hash', '')}"
                f"{' ' + mode if mode else ''}\n"
            )
        # An added file has no minus_file and a deleted file no plus_file;
        # git apply wants /dev/null on that side, and rejects a hunk that has
        # no file headers at all.
        minus, plus = file_diff.get("minus_file"), file_diff.get("plus_file")
        hunks = file_diff.get("hunks") or []
        if minus or plus or hunks:
            minus_path = (minus or {}).get("path") or "/dev/null"
            plus_path = (plus or {}).get("path") or "/dev/null"
            chunks.append(f"--- {minus_path}\n+++ {plus_path}\n")
        for hunk in hunks:
            desc = hunk.get("descriptor") or {}
            chunks.append(f"@@ -{_range(desc.get('old_range'))} +{_range(desc.get('new_range'))} @@")
            chunks.append(f" {desc['section']}\n" if desc.get("section") else "\n")
            for line in (hunk.get("line_group") or {}).get("all_lines", []):
                prefix = {"context": " ", "added": "+", "deleted": "-", "note": "\\ "}.get(
                    line.get("type", "")
                )
                if prefix is not None:
                    chunks.append(f"{prefix}{line.get('content', '')}\n")
    return "".join(chunks)


def build_r2e_commands(
    patch: str,
    expected_output_json: str | dict[str, Any],
    *,
    testbed: str = "/testbed",
    hidden_tests: str = "/r2e_tests",
    archive: str = "/tmp/r2e_tests.tar.gz",
    venv_bin: str = "/testbed/.venv/bin",
    local_bin: str = "/root/.local/bin",
    work_dir: str = "/tmp",
) -> tuple[str, str, str]:
    """Build (setup_command, reset_command, command) for an R2E-Gym instance."""
    expected_obj = (
        json.loads(expected_output_json)
        if isinstance(expected_output_json, str)
        else expected_output_json
    )
    if not expected_obj:
        raise ValueError("expected_output_json must not be empty")

    expected_bytes = json.dumps(expected_obj, separators=(",", ":"), sort_keys=True).encode("utf-8")
    patch_bytes = patch.encode("utf-8")
    grader_bytes = _INLINE_GRADER.encode("utf-8")

    hidden_clean = hidden_tests.rstrip("/")
    hidden_parent = posixpath.dirname(hidden_clean) or "/"
    hidden_name = posixpath.basename(hidden_clean)

    q_testbed = shlex.quote(testbed)
    q_hidden = shlex.quote(hidden_clean)
    q_hidden_parent = shlex.quote(hidden_parent)
    q_hidden_name = shlex.quote(hidden_name)
    q_archive = shlex.quote(archive)
    q_venv_bin = shlex.quote(venv_bin)
    q_local_bin = shlex.quote(local_bin)
    q_venv_py = shlex.quote(posixpath.join(venv_bin, "python"))
    q_restored = shlex.quote(posixpath.join(testbed, hidden_name))
    q_patch_file = shlex.quote(posixpath.join(work_dir, "r2e_gold.patch"))
    q_expected_file = shlex.quote(posixpath.join(work_dir, "r2e_expected.json"))
    q_grader_file = shlex.quote(posixpath.join(work_dir, "r2e_grade.py"))
    q_eval_log = shlex.quote(posixpath.join(work_dir, "r2e_eval.log"))

    setup_command = (
        f"set -e && test -d {q_testbed} && "
        f"if [ -d {q_venv_bin} ]; then "
        f"mkdir -p {q_local_bin} && "
        f'for f in {q_venv_bin}/*; do if [ -f "$f" ] && [ -x "$f" ]; then ln -sfn "$f" {q_local_bin}/"$(basename "$f")"; fi; done && '
        f"(ln -sfn {q_venv_py} /usr/local/bin/python 2>/dev/null || true) && "
        f"(ln -sfn {q_venv_py} /usr/local/bin/python3 2>/dev/null || true); "
        f"fi && "
        f"if [ -d {q_hidden} ]; then tar -C {q_hidden_parent} -czf {q_archive} {q_hidden_name} && rm -rf {q_hidden}; fi && "
        f"test -f {q_archive} && "
        f"cd {q_testbed} && (timeout 60 find . -name '__pycache__' -prune -exec rm -rf {{}} + 2>/dev/null || true) && "
        f"git -c user.email=bench@localhost -c user.name=bench add -A && "
        f'git -c user.email=bench@localhost -c user.name=bench commit -q --allow-empty -m "benchmark baseline"'
    )
    reset_command = (
        f"cd {q_testbed} && git reset -q --hard && git clean -qfd && rm -rf {q_restored}"
    )
    command = "\n".join([
        "set -e",
        (
            f'export PATH={q_venv_bin}:{q_local_bin}:"${{PATH:+$PATH:}}"/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin '
            "PAGER=cat GIT_PAGER=cat MANPAGER=cat PIP_PROGRESS_BAR=off TQDM_DISABLE=1 CI=1"
        ),
        f'PY={q_venv_py}; [ -x "$PY" ] || PY=python3',
        _heredoc_b64_write(patch_bytes, "__R2E_PATCH_B64__", q_patch_file),
        _heredoc_b64_write(expected_bytes, "__R2E_EXPECTED_B64__", q_expected_file),
        _heredoc_b64_write(grader_bytes, "__R2E_GRADER_B64__", q_grader_file),
        f"cd {q_testbed}",
        f"git apply --whitespace=fix {q_patch_file}",
        f"rm -rf {q_restored}",
        f"tar -C {q_testbed} -xzf {q_archive}",
        f"(bash run_tests.sh > {q_eval_log} 2>&1 || true)",
        f'"$PY" {q_grader_file} {q_eval_log} {q_expected_file}',
    ])
    return setup_command, reset_command, command


def _unwrap_example(raw: dict[str, Any]) -> dict[str, Any]:
    if "responses_create_params" in raw:
        inst = raw["responses_create_params"]["metadata"]["instance_dict"]
        return json.loads(inst) if isinstance(inst, str) else dict(inst)
    return dict(raw)


def _read_local_rows(path: Path) -> list[dict[str, Any]]:
    text = path.read_text(encoding="utf-8")
    if path.suffix == ".json":
        items = json.loads(text)
        return [_unwrap_example(item) for item in items]
    return [_unwrap_example(json.loads(line)) for line in text.splitlines() if line.strip()]


def load_r2e_rows(
    source: str, dataset: str, split: str, revision: str | None
) -> tuple[list[dict[str, Any]], str]:
    """Load R2E-Gym rows and return (rows, resolved_revision)."""
    resolved = resolve_revision(dataset, revision)
    if resolved is None:
        raise RuntimeError(
            "cannot resolve dataset revision: huggingface_hub is missing; pip install datasets"
        )
    if dataset.endswith((".json", ".jsonl")):
        return _read_local_rows(Path(dataset)), resolved
    if source == "r2e_gym_easy":
        from huggingface_hub import hf_hub_download

        local_file = hf_hub_download(
            dataset, R2E_EASY_FILES[split], repo_type="dataset", revision=resolved
        )
        return _read_local_rows(Path(local_file)), resolved
    from datasets import load_dataset

    return [dict(row) for row in load_dataset(dataset, split=split, revision=resolved)], resolved


def _expected_tests(expected_output_json: Any) -> dict[str, Any]:
    """The expected test map of a row; empty when the row has nothing to grade against."""
    if isinstance(expected_output_json, str):
        if not expected_output_json.strip():
            return {}
        expected_output_json = json.loads(expected_output_json)
    return dict(expected_output_json or {})


def _instance_id(example: dict[str, Any]) -> str:
    iid = str(example.get("instance_id") or "").strip()
    if not iid:
        iid = f"{example.get('repo_name', 'repo')}-{example.get('commit_hash', '')}".strip("-")
    return iid


def _read_exclusions(path: str | None) -> set[str]:
    if not path:
        return set()
    excluded: set[str] = set()
    for line in Path(path).read_text(encoding="utf-8").splitlines():
        tokens = line.split("#", 1)[0].strip().split()
        if tokens:
            excluded.add(tokens[0])
    return excluded


def _resolve_pinned_image(
    ref: str, digests: dict[str, str] | None, registry: str | None
) -> str | None:
    if "@sha256:" in ref:
        return pin_ref(ref)
    if digests is not None:
        mapped = digests.get(ref) or digests.get("docker.io/" + ref)
        return pin_ref(mapped) if mapped else None
    if registry:
        rewritten = f"{registry.rstrip('/')}/{ref.removeprefix('docker.io/')}"
        return pin_ref(rewritten)
    return None


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument(
        "--source",
        default="r2e_gym_easy",
        choices=["r2e_gym_easy", "r2e_gym"],
        help="R2E-Gym dataset layout (default: r2e_gym_easy).",
    )
    ap.add_argument(
        "--dataset",
        default=None,
        help="Hugging Face dataset ID or local .json/.jsonl path.",
    )
    ap.add_argument("--split", default="train", help="Dataset split (default: train).")
    ap.add_argument("--revision", default=None, help="Dataset revision (commit, tag, or branch).")
    ap.add_argument(
        "--limit",
        type=int,
        default=0,
        help="Stop after N instances in --out (0 = all). Not with --images-out alone: that list carries every candidate image, because the manifest pass skips tags that share a digest and reads past the first N candidates.",
    )
    ap.add_argument("--exclude-file", default=None, help="File of instance IDs to skip.")
    ap.add_argument(
        "--images-out",
        default=None,
        help="Write unique sorted task image references to this file.",
    )
    ap.add_argument(
        "--image-digests",
        default=None,
        help="JSON mapping {tag_ref: pinned_ref} from resolve_images.sh.",
    )
    ap.add_argument(
        "--registry",
        default=None,
        help="Registry prefix for resolving tags directly with crane when --image-digests is omitted.",
    )
    ap.add_argument(
        "--out",
        default=None,
        help="JSONL manifest to write (required unless --images-out is used alone).",
    )
    args = ap.parse_args(argv)

    if not args.out and not args.images_out:
        ap.error("at least one of --out or --images-out is required")
    if not args.out and args.limit:
        ap.error(
            "--limit applies to --out; --images-out alone lists every candidate image "
            "so the manifest pass can skip tags that share a digest"
        )

    dataset = args.dataset or (
        R2E_EASY_DATASET if args.source == "r2e_gym_easy" else R2E_SUBSET_DATASET
    )
    try:
        raw_rows, resolved = load_r2e_rows(args.source, dataset, args.split, args.revision)
    except (OSError, ValueError, KeyError, RuntimeError, ImportError) as exc:
        print(f"cannot load {dataset}: {exc}", file=sys.stderr)
        return 2

    # Both passes select from the same candidates, filtered the same way, so
    # every tag the manifest pass reads is in the image list of the first.
    excluded = _read_exclusions(args.exclude_file)
    candidates: list[tuple[str, dict[str, Any], str]] = []
    skipped_empty_patch = skipped_empty_expected = 0
    for row in raw_rows:
        iid = _instance_id(row)
        if not iid or iid in excluded:
            continue
        patch = extract_gold_patch(row.get("parsed_commit_content", ""))
        if not patch.strip():
            skipped_empty_patch += 1
            continue
        if not _expected_tests(row.get("expected_output_json")):
            skipped_empty_expected += 1
            continue
        candidates.append((iid, row, patch))
    candidates.sort(key=lambda item: item[0])

    if args.images_out and not args.out:
        seen_refs = sorted({str(row["docker_image"]).strip() for _, row, _ in candidates})
        Path(args.images_out).write_text("\n".join(seen_refs) + ("\n" if seen_refs else ""), encoding="utf-8")
        print(
            f"wrote {len(seen_refs)} image references to {args.images_out}; "
            f"{skipped_empty_patch} empty gold patches and {skipped_empty_expected} empty "
            f"expected_output_json skipped",
            file=sys.stderr,
        )
        return 0

    digests: dict[str, str] | None = None
    if args.image_digests:
        digests = json.loads(Path(args.image_digests).read_text(encoding="utf-8"))

    written = missing = 0
    digest_seen: dict[str, str] = {}
    selected_refs: set[str] = set()
    assert args.out is not None
    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with out_path.open("w", encoding="utf-8") as dst:
        for iid, row, patch in candidates:
            if args.limit and written >= args.limit:
                break
            ref = str(row["docker_image"]).strip()
            pinned = _resolve_pinned_image(ref, digests, args.registry)
            if pinned is None:
                print(f"missing image for {iid}: {ref}", file=sys.stderr)
                missing += 1
                continue
            digest = pinned.rsplit("@", 1)[1]
            if digest in digest_seen:
                print(
                    f"duplicate image for {iid}: {ref} shares digest with {digest_seen[digest]}, skipping",
                    file=sys.stderr,
                )
                continue
            setup_cmd, reset_cmd, grade_cmd = build_r2e_commands(
                patch, row["expected_output_json"]
            )
            digest_seen[digest] = iid
            selected_refs.add(ref)
            rec = {
                "kind": "command",
                "instance_id": iid,
                "repo": str(row.get("repo_name", "")),
                "image": pinned,
                "setup_command": setup_cmd,
                "reset_command": reset_cmd,
                "command": grade_cmd,
                "expected_exit_code": 0,
            }
            dst.write(json.dumps(rec) + "\n")
            written += 1

    if args.images_out:
        sorted_refs = sorted(selected_refs)
        Path(args.images_out).write_text(
            "\n".join(sorted_refs) + ("\n" if sorted_refs else ""), encoding="utf-8"
        )

    meta = {
        "source": args.source,
        "dataset": dataset,
        "split": args.split,
        "revision": args.revision,
        "resolved_revision": resolved,
        "registry": args.registry,
        "image_digests": args.image_digests,
        "instances": written,
        "missing_images": missing,
        "skipped_empty_patch": skipped_empty_patch,
        "skipped_empty_expected": skipped_empty_expected,
        "generated_at": datetime.datetime.now(datetime.UTC).isoformat(timespec="seconds"),
    }
    Path(args.out + ".meta.json").write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")
    print(
        f"wrote {written} R2E-Gym instances to {args.out}; {missing} missing images; "
        f"{skipped_empty_patch} empty gold patches and {skipped_empty_expected} empty "
        f"expected_output_json skipped",
        file=sys.stderr,
    )
    return 1 if missing else 0


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
