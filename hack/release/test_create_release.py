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

"""Unit tests for hack/release/create-release.py."""

import importlib.util
import subprocess
import sys
from pathlib import Path
from unittest.mock import patch

import pytest

_SPEC = importlib.util.spec_from_file_location(
    "create_release",
    Path(__file__).with_name("create-release.py"),
)
assert _SPEC is not None and _SPEC.loader is not None
cr = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(cr)


@pytest.mark.parametrize(
    ("tag", "expected"),
    [
        ("v0.1.0", "0.1.0"),
        ("v1.2.3", "1.2.3"),
        ("v0.1.0-rc1", "0.1.0rc1"),
        ("v0.1.0-rc.2", "0.1.0rc2"),
        ("v0.1.0-c3", "0.1.0rc3"),
        ("v0.1.0-alpha1", "0.1.0a1"),
        ("v0.1.0-a.4", "0.1.0a4"),
        ("v0.1.0-beta2", "0.1.0b2"),
        ("v0.1.0-b.5", "0.1.0b5"),
        ("v0.1.0-dev0", "0.1.0.dev0"),
        ("v0.1.0-dev.7", "0.1.0.dev7"),
    ],
)
def test_pep440_version_valid(tag: str, expected: str) -> None:
    assert cr.SEMVER_REGEX.match(tag)
    assert cr.pep440_version(tag) == expected


@pytest.mark.parametrize(
    "invalid_tag",
    [
        "0.1.0",
        "v0.1",
        "v0.1.0-foo",
        "v0.1.0-rc",
        "v0.1.0+local",
        "release-v0.1.0",
    ],
)
def test_pep440_subcommand_rejects_invalid_tag(invalid_tag: str, capsys: pytest.CaptureFixture[str]) -> None:
    assert cr.SEMVER_REGEX.match(invalid_tag) is None
    with patch.object(sys, "argv", ["create-release.py", "pep440", "--tag", invalid_tag]), pytest.raises(SystemExit) as exc:
        cr.main()
    assert exc.value.code == 1
    assert "not a valid PEP 440-compatible semver tag" in capsys.readouterr().err


def test_pep440_subcommand_prints_version(capsys: pytest.CaptureFixture[str]) -> None:
    with patch.object(sys, "argv", ["create-release.py", "pep440", "--tag", "v0.2.0-rc1"]):
        cr.main()
    assert capsys.readouterr().out.strip() == "0.2.0rc1"


@pytest.mark.parametrize(
    ("body", "expected"),
    [
        (None, None),
        ("", None),
        ("## Release note\n```release-note\nNONE\n```", None),
        ("## Release note\n```release-note\nn/a\n```", None),
        (
            "## Release note\n<!-- guide -->\n```release-note\nAdd PyPI Trusted Publishing.\n```",
            "Add PyPI Trusted Publishing.",
        ),
        ("## Release note\n```\nFix guest PTY resize race.\n```", "Fix guest PTY resize race."),
    ],
)
def test_extract_release_note(body: str | None, expected: str | None) -> None:
    assert cr.extract_release_note(body) == expected


def test_preserve_images_section() -> None:
    notes = "# Release v0.1.0\n\n### Highlights\n- Item 1"
    existing_with_images = (
        "# Old Draft\r\n\r\n## Images\r\n\r\n- `ghcr.io/example/api@sha256:abc`\r\n\r\n## Contributors\r\n@alice\r\n"
    )
    merged = cr.preserve_images_section(notes, existing_with_images)
    assert merged == "# Release v0.1.0\n\n### Highlights\n- Item 1\n\n## Images\n\n- `ghcr.io/example/api@sha256:abc`\n"

    # Existing body without ## Images leaves new notes unchanged.
    assert cr.preserve_images_section(notes, "# Old Draft\n\nNo images yet.") == notes + "\n"

    # New notes that already define ## Images do not duplicate the section.
    notes_with_images = notes + "\n\n## Images\n\n- `ghcr.io/example/custom@sha256:def`"
    assert cr.preserve_images_section(notes_with_images, existing_with_images) == notes_with_images + "\n"


def test_extract_changelog_unreleased(tmp_path: Path) -> None:
    assert cr.extract_changelog_unreleased(tmp_path) is None

    changelog = tmp_path / "CHANGELOG.md"
    changelog.write_text("# Changelog\n\n## Unreleased\n\n## v0.0.10\n\n- Older entry\n", encoding="utf-8")
    assert cr.extract_changelog_unreleased(tmp_path) is None

    changelog.write_text(
        "# Changelog\n\n## Unreleased\n\n### Added\n\n- New telemetry flag\n\n## v0.0.10\n\n- Older entry\n",
        encoding="utf-8",
    )
    assert cr.extract_changelog_unreleased(tmp_path) == "### Added\n\n- New telemetry flag"


def test_parse_go_mod(tmp_path: Path) -> None:
    (tmp_path / "go.mod").write_text(
        "module github.com/agent-substrate/env\n\n"
        "go 1.27.5 // comment\n\n"
        "require go.ate.dev/substrate v0.6.1-0.20261028195031-475f49b02335\n"
        "require github.com/example/indirect-single v1.0.0 // indirect\n\n"
        "require (\n"
        "\tgoogle.golang.org/grpc v1.84.0\n"
        "\tk8s.io/api v0.37.1 // pin\n"
        "\tgolang.org/x/sys v0.30.0 // indirect\n"
        ")\n",
        encoding="utf-8",
    )
    go_ver, reqs = cr.parse_go_mod(tmp_path)
    assert go_ver == "1.27.5"
    assert reqs == {
        "go.ate.dev/substrate": "v0.6.1-0.20261028195031-475f49b02335",
        "google.golang.org/grpc": "v1.84.0",
        "k8s.io/api": "v0.37.1",
    }


def test_detect_repo_fails_loudly_when_unparseable(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("GITHUB_REPOSITORY", raising=False)
    assert cr.detect_repo("owner/explicit") == "owner/explicit"

    fake_ok = subprocess.CompletedProcess(["git"], 0, "git@github.com:diamondburned/sandboxenv-rl.git\n", "")
    with patch.object(cr, "run_cmd", return_value=fake_ok):
        assert cr.detect_repo() == "diamondburned/sandboxenv-rl"

    fake_bad = subprocess.CompletedProcess(["git"], 1, "", "")
    with patch.object(cr, "run_cmd", return_value=fake_bad), pytest.raises(SystemExit) as exc:
        cr.detect_repo()
    assert exc.value.code == 1


def test_draft_dry_run_prints_edit_when_draft_exists(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    notes = tmp_path / "NOTES.md"
    notes.write_text("# v0.1.0\n", encoding="utf-8")

    def fake_run_cmd(cmd: list[str], check: bool = True, cwd: Path | None = None) -> subprocess.CompletedProcess[str]:
        if cmd[:2] == ["git", "status"]:
            return subprocess.CompletedProcess(cmd, 0, "", "")
        if cmd[:3] == ["git", "rev-parse", "--verify"]:
            return subprocess.CompletedProcess(cmd, 0, "a" * 40 + "\n", "")
        if cmd[:4] == ["git", "rev-parse", "-q", "--verify"]:
            if "refs/remotes/" in cmd[4]:
                return subprocess.CompletedProcess(cmd, 0, "a" * 40 + "\n", "")
            return subprocess.CompletedProcess(cmd, 1, "", "")
        if cmd[:3] == ["git", "merge-base", "--is-ancestor"]:
            return subprocess.CompletedProcess(cmd, 0, "", "")
        return subprocess.CompletedProcess(cmd, 1, "", "")

    with (
        patch.object(cr, "run_cmd", side_effect=fake_run_cmd),
        patch.object(cr, "fetch_release_by_tag", return_value={"isDraft": True, "url": "https://example/draft", "body": ""}),
        patch.object(
            sys,
            "argv",
            ["create-release.py", "draft", "--dry-run", "--repo", "owner/repo", "--tag", "v0.1.0", "--notes-file", str(notes)],
        ),
    ):
        cr.main()

    out = capsys.readouterr().out
    assert "gh release edit v0.1.0 --repo owner/repo --draft" in out
    assert "gh release create" not in out


def test_main_reports_called_process_error_stderr(capsys: pytest.CaptureFixture[str]) -> None:
    err = subprocess.CalledProcessError(1, ["gh", "release", "create"], output="", stderr="HTTP 422: Validation Failed\n")
    with (
        patch.object(cr, "handle_pep440", side_effect=err),
        patch.object(sys, "argv", ["create-release.py", "pep440", "--tag", "v0.1.0"]),
        pytest.raises(SystemExit) as exc,
    ):
        cr.main()
    assert exc.value.code == 1
    captured_err = capsys.readouterr().err
    assert "gh release create" in captured_err
    assert "HTTP 422: Validation Failed" in captured_err
