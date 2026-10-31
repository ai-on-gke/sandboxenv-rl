#!/usr/bin/env python3

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

"""Release automation tool for Agent Substrate Environments (sandboxenv-rl).

Subcommands:
  context: Gathers PR metadata, GitHub generated release notes, and contributor lists
           into a structured context file for drafting release notes.
  draft:   Verifies release notes, creates/pushes the git tag, and creates a draft
           GitHub Release (which triggers container build and CLI binary packaging CI).
"""

import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path

SEMVER_REGEX = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.]+)?$")
RELEASE_NOTE_BLOCK_REGEX = re.compile(
    r"(?i)##\s*Release\s*note\s*[\r\n]+(?:<!--[\s\S]*?-->\s*)*```(?:release-note)?\s*([\s\S]*?)\s*```"
)


def run_cmd(cmd: list[str], check: bool = True, cwd: Path | None = None) -> subprocess.CompletedProcess[str]:
    """Runs a command and returns CompletedProcess with captured text output."""
    return subprocess.run(
        cmd,
        cwd=cwd,
        capture_output=True,
        text=True,
        check=check,
    )


def detect_repo(repo_arg: str | None = None, worktree_dir: Path | None = None) -> str:
    """Detects GitHub repository owner/name."""
    if repo_arg:
        return repo_arg

    env_repo = os.environ.get("GITHUB_REPOSITORY")
    if env_repo:
        return env_repo

    target_dir = worktree_dir or Path(__file__).resolve().parent.parent.parent
    try:
        proc = run_cmd(["git", "-C", str(target_dir), "config", "--get", "remote.origin.url"], check=False)
        url = proc.stdout.strip()
        match = re.search(r"github\.com[:/]([a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+)(?:\.git)?$", url)
        if match:
            return match.group(1).removesuffix(".git")
    except (subprocess.SubprocessError, OSError) as e:
        print(f"Warning: Failed to detect repo from git remote: {e}", file=sys.stderr)

    return "ai-on-gke/sandboxenv-rl"


def extract_release_note(pr_body: str | None) -> str | None:
    """Extracts the ```release-note block from PR body if present and non-trivial."""
    if not pr_body:
        return None
    match = RELEASE_NOTE_BLOCK_REGEX.search(pr_body)
    if not match:
        return None
    raw_note = match.group(1).strip()
    if not raw_note or raw_note.upper() in ("NONE", "N/A", "NO"):
        return None
    return raw_note


def extract_pr_summary(pr_body: str | None) -> str | None:
    """Extracts a short summary from the '## What' section of the PR body if available."""
    if not pr_body:
        return None
    match = re.search(r"(?i)##\s*What\s*[\r\n]+([\s\S]*?)(?=##|\Z)", pr_body)
    if not match:
        return None
    lines = [line.strip() for line in match.group(1).strip().splitlines() if line.strip() and not line.strip().startswith("<!--")]
    if not lines:
        return None
    # Return first 2 non-empty bullet points or sentences
    return " ".join(lines[:2])


def fetch_merged_prs(repo: str) -> dict[int, dict]:
    """Fetches recently merged PRs from GitHub as a lookup dict keyed by PR number."""
    cmd = [
        "gh", "pr", "list",
        "--repo", repo,
        "--state", "merged",
        "--limit", "150",
        "--json", "number,title,author,labels,body,url,mergedAt",
    ]
    proc = run_cmd(cmd, check=False)
    if proc.returncode != 0:
        print(f"Warning: Failed to fetch PR list via 'gh': {proc.stderr.strip()}", file=sys.stderr)
        return {}

    try:
        prs_list = json.loads(proc.stdout)
        return {pr["number"]: pr for pr in prs_list}
    except json.JSONDecodeError as e:
        print(f"Warning: Failed to parse PR JSON: {e}", file=sys.stderr)
        return {}


def handle_context(args: argparse.Namespace) -> None:
    """Gathers release context and writes to output file."""
    repo = detect_repo(args.repo)
    tag = args.tag
    if not SEMVER_REGEX.match(tag):
        print(f"Error: Tag '{tag}' is not a valid semver tag (expected format vX.Y.Z or vX.Y.Z-rc1)", file=sys.stderr)
        sys.exit(1)

    print(f"Collecting release notes context for {repo} tag {tag}...")

    # Call GitHub generate-notes API
    gen_cmd = [
        "gh", "api",
        "--method", "POST",
        "-H", "Accept: application/vnd.github+json",
        f"/repos/{repo}/releases/generate-notes",
        "-f", f"tag_name={tag}",
    ]
    if args.previous_tag:
        gen_cmd.extend(["-f", f"previous_tag_name={args.previous_tag}"])
    if args.target_commitish:
        gen_cmd.extend(["-f", f"target_commitish={args.target_commitish}"])

    gen_proc = run_cmd(gen_cmd, check=False)
    if gen_proc.returncode != 0:
        print(f"Error calling GitHub generate-notes API:\n{gen_proc.stderr}", file=sys.stderr)
        sys.exit(1)

    gen_data = json.loads(gen_proc.stdout)
    gh_notes = gen_data.get("body", "")

    # Extract all PR numbers referenced in generated notes
    pr_numbers = [int(n) for n in re.findall(r"/pull/(\d+)", gh_notes)]
    unique_pr_nums = sorted(set(pr_numbers), reverse=True)

    print(f"Found {len(unique_pr_nums)} PRs in generated notes range. Fetching metadata...")
    all_prs = fetch_merged_prs(repo)

    # Build detailed PR context
    prs_context = []
    contributors = set()

    for num in unique_pr_nums:
        pr = all_prs.get(num)
        if not pr:
            # Fallback to single fetch
            single_proc = run_cmd(
                ["gh", "pr", "view", str(num), "--repo", repo, "--json", "number,title,author,labels,body,url"],
                check=False,
            )
            if single_proc.returncode == 0:
                try:
                    pr = json.loads(single_proc.stdout)
                except json.JSONDecodeError as e:
                    print(f"Warning: Failed to parse PR #{num} JSON: {e}", file=sys.stderr)

        if pr:
            author_login = pr.get("author", {}).get("login", "unknown")
            if not pr.get("author", {}).get("is_bot", False) and author_login != "app/dependabot":
                contributors.add(f"@{author_login}")

            labels = [lbl.get("name", "") for lbl in pr.get("labels", []) if lbl.get("name")]
            rel_note = extract_release_note(pr.get("body"))
            summary = extract_pr_summary(pr.get("body"))

            prs_context.append({
                "number": pr["number"],
                "title": pr["title"],
                "author": f"@{author_login}",
                "url": pr.get("url", f"https://github.com/{repo}/pull/{num}"),
                "labels": labels,
                "release_note": rel_note,
                "summary": summary,
            })
        else:
            prs_context.append({
                "number": num,
                "title": f"PR #{num}",
                "author": "@unknown",
                "url": f"https://github.com/{repo}/pull/{num}",
                "labels": [],
                "release_note": None,
                "summary": None,
            })

    # Read base template if available
    template_path = Path(__file__).parent / "release-template.md"
    template_str = template_path.read_text(encoding="utf-8") if template_path.exists() else ""

    # Assemble structured context output
    output_lines = [
        f"# Release Context for {tag}",
        "",
        f"- **Repository**: `{repo}`",
        f"- **Target Tag**: `{tag}`",
        f"- **Target Commitish**: `{args.target_commitish or 'default branch (HEAD)'}`",
        f"- **Previous Tag**: `{args.previous_tag or 'auto-detected by GitHub'}`",
        "",
        "## GitHub Generated Notes",
        "```markdown",
        gh_notes.strip(),
        "```",
        "",
        "## Extracted Pull Request Details",
        "",
    ]

    for p in prs_context:
        label_str = f" `[{', '.join(p['labels'])}]`" if p["labels"] else ""
        output_lines.append(f"### PR #{p['number']}: {p['title']}{label_str}")
        output_lines.append(f"- Author: {p['author']} ({p['url']})")
        if p["release_note"]:
            output_lines.append(f"- **Release Note**: {p['release_note']}")
        if p["summary"]:
            output_lines.append(f"- **Summary**: {p['summary']}")
        output_lines.append("")

    sorted_contributors = sorted(contributors, key=lambda s: s.lower())
    output_lines.extend([
        "## Contributors",
        "",
        ", ".join(sorted_contributors) if sorted_contributors else "None",
        "",
        "## Release Notes Template Reference",
        "Use the following template structure (`hack/release/release-template.md`) to synthesize the final release notes:",
        "```markdown",
        template_str.strip(),
        "```",
    ])

    out_content = "\n".join(output_lines) + "\n"

    out_file = Path(args.output)
    out_file.parent.mkdir(parents=True, exist_ok=True)
    out_file.write_text(out_content, encoding="utf-8")
    print(f"Successfully generated release context: {out_file.resolve()}")


def handle_draft(args: argparse.Namespace) -> None:
    """Verifies notes, pushes tag, and creates draft GitHub release."""
    repo = detect_repo(args.repo)
    tag = args.tag
    if not SEMVER_REGEX.match(tag):
        print(f"Error: Tag '{tag}' is not a valid semver tag (expected format vX.Y.Z or vX.Y.Z-rc1)", file=sys.stderr)
        sys.exit(1)

    notes_path = Path(args.notes_file)
    if not notes_path.exists():
        print(f"Error: Release notes file '{notes_path}' does not exist", file=sys.stderr)
        sys.exit(1)

    notes_content = notes_path.read_text(encoding="utf-8").strip()
    if not notes_content:
        print(f"Error: Release notes file '{notes_path}' is empty", file=sys.stderr)
        sys.exit(1)

    remote = args.remote
    commit = args.target_commitish or "HEAD"

    # Check if a published (non-draft) release already exists for this tag
    check_cmd = ["gh", "api", f"/repos/{repo}/releases/tags/{tag}"]
    proc = run_cmd(check_cmd, check=False)
    if proc.returncode == 0:
        try:
            rel = json.loads(proc.stdout)
            if not rel.get("draft", False):
                print(f"Error: Tag '{tag}' is already an officiated (published) release on {repo} ({rel.get('html_url')}).", file=sys.stderr)
                print("Cannot overwrite an existing published release.", file=sys.stderr)
                sys.exit(1)
        except json.JSONDecodeError:
            print(f"Warning: Failed to parse release JSON for tag {tag}", file=sys.stderr)

    print(f"=== Creating / Updating Draft Release {tag} ===")
    print(f"- Repository: {repo}")
    print(f"- Git Remote: {remote}")
    print(f"- Target Commitish: {commit}")
    print(f"- Notes File: {notes_path} ({len(notes_content.splitlines())} lines)")

    if args.dry_run:
        print("\n[DRY RUN] Would execute the following steps:")
        print(f"1. git tag -a {tag} {commit} -m \"Release {tag}\" (if not already tagged)")
        print(f"2. git push {remote} refs/tags/{tag}")
        print(f"3. gh release create {tag} --draft --verify-tag --notes-file {notes_path} --title \"{tag}\" --target \"{commit}\"")
        return

    # Check if local tag exists; if not, create annotated tag
    tag_check = run_cmd(["git", "rev-parse", "-q", "--verify", f"refs/tags/{tag}"], check=False)
    if tag_check.returncode != 0:
        print(f"Creating local annotated tag '{tag}' at commit '{commit}'...")
        run_cmd(["git", "tag", "-a", tag, commit, "-m", f"Release {tag}"])
    else:
        print(f"Local tag '{tag}' already exists.")

    # Push tag to remote
    print(f"Pushing tag '{tag}' to remote '{remote}'...")
    push_proc = run_cmd(["git", "push", remote, f"refs/tags/{tag}"])
    if push_proc.returncode != 0:
        print(f"Error pushing tag to remote:\n{push_proc.stderr}", file=sys.stderr)
        sys.exit(1)

    # Check if a draft release exists for this tag
    draft_check = run_cmd(["gh", "api", f"/repos/{repo}/releases/tags/{tag}"], check=False)
    if draft_check.returncode == 0:
        print(f"Updating existing draft release notes for '{tag}'...")
        edit_cmd = [
            "gh", "release", "edit", tag,
            "--repo", repo,
            "--draft",
            "--title", tag,
            "--notes-file", str(notes_path.resolve()),
        ]
        run_cmd(edit_cmd)
        rel_url = f"https://github.com/{repo}/releases/tag/{tag}"
    else:
        print(f"Creating new draft release '{tag}' on {repo}...")
        create_cmd = [
            "gh", "release", "create", tag,
            "--repo", repo,
            "--draft",
            "--verify-tag",
            "--title", tag,
            "--notes-file", str(notes_path.resolve()),
        ]
        run_cmd(create_cmd)
        rel_url = f"https://github.com/{repo}/releases/tag/{tag}"

    print("\n✅ Draft release successfully created/updated!")
    print(f"   Draft URL: {rel_url}")
    print("\nNext Automated CI Actions (kicked off by tag push):")
    print("  1. GitHub Actions workflow .github/workflows/release.yaml is building:")
    print("     - Multi-arch container images: ate-env-api, ate-env-guest, oh-agent-server-layer")
    print("     - Signing images with Cosign and pushing to GHCR (and optional GAR)")
    print("     - Cross-compiling ate-env CLI binaries for Linux and macOS (amd64 / arm64)")
    print("     - Generating SHA256SUMS_${TAG}.txt checksums and Cosign signatures")
    print("     - Uploading CLI assets and image verification tables to the draft release")
    print("\nNext Human Action:")
    print("  2. Review the draft release in GitHub UI and click 'Publish release'.")
    print("     Publishing the release automatically triggers PyPI publication via")
    print("     OIDC Trusted Publishing for all Python client and integration distributions.")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Release automation tool for Agent Substrate Environments (sandboxenv-rl)",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    # Subcommand: context
    ctx_parser = subparsers.add_parser(
        "context",
        help="Gathers PR metadata, GitHub release notes, and contributors for drafting notes",
    )
    ctx_parser.add_argument("--tag", required=True, help="Target release tag (e.g. v0.1.0)")
    ctx_parser.add_argument("--previous-tag", help="Previous release tag to diff against")
    ctx_parser.add_argument("--target-commitish", help="Commitish value to tag (branch or SHA, defaults to HEAD)")
    ctx_parser.add_argument("--repo", help="GitHub repo owner/name (e.g. ai-on-gke/sandboxenv-rl)")
    ctx_parser.add_argument(
        "--output",
        default="hack/release/release-context.md",
        help="Path to write the gathered context markdown (default: hack/release/release-context.md)",
    )

    # Subcommand: draft
    draft_parser = subparsers.add_parser(
        "draft",
        help="Verifies release notes, pushes git tag, and creates draft GitHub release",
    )
    draft_parser.add_argument("--tag", required=True, help="Target release tag (e.g. v0.1.0)")
    draft_parser.add_argument(
        "--notes-file",
        required=True,
        help="Path to finalized release notes markdown file",
    )
    draft_parser.add_argument("--target-commitish", help="Target commitish to tag (defaults to HEAD)")
    draft_parser.add_argument("--remote", default="origin", help="Git remote to push tag to (default: origin)")
    draft_parser.add_argument("--repo", help="GitHub repo owner/name (e.g. ai-on-gke/sandboxenv-rl)")
    draft_parser.add_argument("--dry-run", action="store_true", help="Print actions without modifying git or GitHub")

    args = parser.parse_args()

    if args.command == "context":
        handle_context(args)
    elif args.command == "draft":
        handle_draft(args)


if __name__ == "__main__":
    main()
