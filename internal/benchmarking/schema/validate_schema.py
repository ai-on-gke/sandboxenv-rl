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

"""Standalone JSON / JSONL validator against canonical v1 JSON Schemas.

Can be invoked by CI, shell scripts, or Python tests without Go installed:
  python3 internal/benchmarking/schema/validate_schema.py \
      --schema trace_event --jsonl path/to/trace.jsonl
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import datetime
from pathlib import Path
from typing import Any

try:
    import jsonschema as _jsonschema_mod  # type: ignore[import-untyped]
except ImportError:
    _jsonschema_mod = None

SCHEMA_DIR = Path(__file__).resolve().parent / "v1"

SCHEMA_MAP = {
    "event": "event.schema.json",
    "placement": "placement.schema.json",
    "report": "report.schema.json",
    "run_window": "run_window.schema.json",
    "schedule": "schedule.schema.json",
    "stats_row": "stats_row.schema.json",
    "system_description": "system_description.schema.json",
    "trace_event": "trace_event.schema.json",
}

_RFC3339_RE = re.compile(
    r"^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$"
)


class SchemaValidationError(ValueError):
    """Raised when a JSON instance violates the target v1 JSON Schema."""


def _is_valid_date_time(val: str) -> bool:
    """Validate RFC3339 date-time format."""
    if not _RFC3339_RE.match(val):
        return False
    try:
        norm = val[:-1] + "+00:00" if val.endswith("Z") else val
        datetime.fromisoformat(norm)
        return True
    except ValueError:
        return False


def _matches_type(instance: Any, expected_type: str) -> bool:
    """Return True if instance matches the JSON Schema primitive type."""
    if expected_type == "integer":
        return isinstance(instance, int) and not isinstance(instance, bool)
    if expected_type == "number":
        return isinstance(instance, (int, float)) and not isinstance(instance, bool)
    type_map: dict[str, type[Any]] = {
        "object": dict,
        "array": list,
        "string": str,
        "boolean": bool,
        "null": type(None),
    }
    target_cls = type_map.get(expected_type)
    return isinstance(instance, target_cls) if target_cls is not None else False


def _resolve_ref(root_schema: dict[str, Any], ref: str) -> dict[str, Any]:
    """Resolve an internal JSON Pointer reference like '#/$defs/foo'."""
    if not ref.startswith("#/"):
        raise SchemaValidationError(f"unsupported external $ref: {ref}")
    target: Any = root_schema
    for part in ref[2:].split("/"):
        if not isinstance(target, dict) or part not in target:
            raise SchemaValidationError(f"unresolvable $ref: {ref}")
        target = target[part]
    if not isinstance(target, dict):
        raise SchemaValidationError(f"$ref {ref} did not resolve to a schema object")
    return target


def _check_scalar_constraints(
    instance: Any, schema: dict[str, Any], path: str
) -> None:
    """Validate const, enum, string, and numeric bounds on instance."""
    if "const" in schema and instance != schema["const"]:
        raise SchemaValidationError(
            f"{path}: expected const {schema['const']!r}, got {instance!r}"
        )
    if "enum" in schema and instance not in schema["enum"]:
        raise SchemaValidationError(
            f"{path}: value {instance!r} not in enum {schema['enum']}"
        )
    if isinstance(instance, str):
        if "minLength" in schema and len(instance) < schema["minLength"]:
            raise SchemaValidationError(
                f"{path}: string length {len(instance)} < minLength {schema['minLength']}"
            )
        if schema.get("format") == "date-time" and not _is_valid_date_time(instance):
            raise SchemaValidationError(
                f"{path}: invalid date-time format {instance!r}"
            )
    if isinstance(instance, (int, float)) and not isinstance(instance, bool):
        if "minimum" in schema and instance < schema["minimum"]:
            raise SchemaValidationError(
                f"{path}: value {instance} < minimum {schema['minimum']}"
            )
        if "exclusiveMinimum" in schema and instance <= schema["exclusiveMinimum"]:
            raise SchemaValidationError(
                f"{path}: value {instance} <= exclusiveMinimum {schema['exclusiveMinimum']}"
            )


def _check_container_constraints(
    instance: Any,
    schema: dict[str, Any],
    root_schema: dict[str, Any],
    path: str,
) -> None:
    """Validate array and object properties recursively."""
    if isinstance(instance, list):
        if "minItems" in schema and len(instance) < schema["minItems"]:
            raise SchemaValidationError(
                f"{path}: array length {len(instance)} < minItems {schema['minItems']}"
            )
        item_schema = schema.get("items")
        if isinstance(item_schema, dict):
            for idx, item in enumerate(instance):
                _validate_stdlib(item, item_schema, root_schema, f"{path}[{idx}]")
        return

    if isinstance(instance, dict):
        for req_key in schema.get("required", []):
            if req_key not in instance:
                raise SchemaValidationError(
                    f"{path}: missing required property {req_key!r}"
                )
        props = schema.get("properties", {})
        addl = schema.get("additionalProperties", True)
        for key, val in instance.items():
            if key in props:
                _validate_stdlib(val, props[key], root_schema, f"{path}.{key}")
            elif addl is False:
                raise SchemaValidationError(
                    f"{path}: unknown additional property {key!r}"
                )
            elif isinstance(addl, dict):
                _validate_stdlib(val, addl, root_schema, f"{path}.{key}")


def _validate_stdlib(
    instance: Any,
    schema: dict[str, Any],
    root_schema: dict[str, Any],
    path: str = "$",
) -> None:
    """Validate instance against a Draft 2020-12 JSON Schema subset using stdlib."""
    if "$ref" in schema:
        target = _resolve_ref(root_schema, schema["$ref"])
        _validate_stdlib(instance, target, root_schema, path)
        return

    expected_type = schema.get("type")
    if expected_type is not None:
        if isinstance(expected_type, list):
            if not any(_matches_type(instance, t) for t in expected_type):
                raise SchemaValidationError(
                    f"{path}: expected type in {expected_type}, got {type(instance).__name__}"
                )
        elif not _matches_type(instance, expected_type):
            raise SchemaValidationError(
                f"{path}: expected type {expected_type}, got {type(instance).__name__}"
            )

    _check_scalar_constraints(instance, schema, path)
    _check_container_constraints(instance, schema, root_schema, path)


def validate_instance(instance: Any, schema: dict[str, Any]) -> None:
    """Validate a decoded JSON value against schema."""
    if _jsonschema_mod is not None:
        validator_cls = _jsonschema_mod.validators.validator_for(schema)
        validator = validator_cls(
            schema, format_checker=_jsonschema_mod.FormatChecker()
        )
        errors = sorted(validator.iter_errors(instance), key=lambda e: list(e.path))
        if errors:
            first = errors[0]
            loc = ".".join(str(p) for p in first.path) or "$"
            raise SchemaValidationError(f"{loc}: {first.message}")
        return
    _validate_stdlib(instance, schema, schema)


def load_schema(schema_name: str, schema_dir: Path = SCHEMA_DIR) -> dict[str, Any]:
    """Load a v1 JSON Schema by logical name, filename, or direct path."""
    direct = Path(schema_name)
    if direct.is_file():
        path = direct
    else:
        filename = SCHEMA_MAP.get(schema_name, schema_name)
        path = schema_dir / filename
    if not path.is_file():
        raise FileNotFoundError(f"schema file not found: {path}")
    with path.open("r", encoding="utf-8") as f:
        data = json.load(f)
    if not isinstance(data, dict):
        raise TypeError(f"schema {path} is not a JSON object")
    return data


def validate_file(
    file_path: Path, schema: dict[str, Any], *, is_jsonl: bool = False
) -> int:
    """Validate a JSON or JSONL file against schema; returns validated record count."""
    text = file_path.read_text(encoding="utf-8")
    if is_jsonl or file_path.suffix == ".jsonl":
        count = 0
        for line_no, raw_line in enumerate(text.splitlines(), start=1):
            stripped = raw_line.strip()
            if not stripped:
                continue
            try:
                obj = json.loads(stripped)
            except json.JSONDecodeError as exc:
                raise SchemaValidationError(
                    f"{file_path}:{line_no}: invalid JSON: {exc}"
                ) from exc
            try:
                validate_instance(obj, schema)
            except SchemaValidationError as exc:
                raise SchemaValidationError(f"{file_path}:{line_no}: {exc}") from exc
            count += 1
        if count == 0:
            raise SchemaValidationError(f"{file_path}: empty JSONL stream")
        return count

    try:
        obj = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SchemaValidationError(f"{file_path}: invalid JSON: {exc}") from exc
    try:
        validate_instance(obj, schema)
    except SchemaValidationError as exc:
        raise SchemaValidationError(f"{file_path}: {exc}") from exc
    return 1


def main(argv: list[str] | None = None) -> int:
    """CLI entrypoint for validating JSON and JSONL files against v1 schemas."""
    parser = argparse.ArgumentParser(
        description="Validate JSON/JSONL artifacts against v1 benchmarking schemas."
    )
    parser.add_argument(
        "--schema",
        default=None,
        help=f"Schema name ({', '.join(sorted(SCHEMA_MAP))}), filename under v1/, or path.",
    )
    parser.add_argument(
        "--schema-dir",
        type=Path,
        default=SCHEMA_DIR,
        help="Directory containing v1/*.schema.json files.",
    )
    parser.add_argument(
        "--jsonl",
        action="store_true",
        help="Treat input files as newline-delimited JSON (JSONL).",
    )
    parser.add_argument(
        "files",
        nargs="+",
        type=Path,
        help="One or more JSON/JSONL files to validate (or <schema> <files...> if --schema is omitted).",
    )
    args = parser.parse_args(argv)

    schema_arg = args.schema
    target_files = list(args.files)
    if schema_arg is None:
        if len(target_files) < 2:
            parser.error("either --schema or positional <schema> <file> is required")
        schema_arg = str(target_files.pop(0))

    try:
        schema = load_schema(schema_arg, args.schema_dir)
        for path in target_files:
            validate_file(path, schema, is_jsonl=args.jsonl)
    except (FileNotFoundError, TypeError, ValueError, SchemaValidationError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
