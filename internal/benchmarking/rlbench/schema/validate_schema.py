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
"""W1 JSON Schema validator helper usable from Python emitters (W11/W13) and CLI."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

SCHEMA_DIR = Path(__file__).resolve().parent / "v1"

SUPPORTED_SCHEMA_KEYWORDS = frozenset(
    {
        "$schema",
        "$id",
        "title",
        "description",
        "$defs",
        "$ref",
        "type",
        "enum",
        "const",
        "minLength",
        "minimum",
        "minItems",
        "items",
        "required",
        "minProperties",
        "properties",
        "additionalProperties",
    }
)


class SchemaValidationError(ValueError):
    """Raised when a JSON/JSONL document violates a W1 JSON Schema."""


def _check_schema_keywords(schema: dict[str, Any], path: str = "$") -> None:
    """Reject unsupported JSON Schema keywords so future schema additions never silently pass."""
    for key in schema:
        if key not in SUPPORTED_SCHEMA_KEYWORDS:
            raise SchemaValidationError(
                f"{path}: unsupported JSON Schema keyword {key!r}"
            )
    defs = schema.get("$defs")
    if isinstance(defs, dict):
        for def_name, def_schema in defs.items():
            if isinstance(def_schema, dict):
                _check_schema_keywords(def_schema, f"{path}.$defs.{def_name}")
    props = schema.get("properties")
    if isinstance(props, dict):
        for prop_name, prop_schema in props.items():
            if isinstance(prop_schema, dict):
                _check_schema_keywords(prop_schema, f"{path}.properties.{prop_name}")
    items = schema.get("items")
    if isinstance(items, dict):
        _check_schema_keywords(items, f"{path}.items")
    addl = schema.get("additionalProperties")
    if isinstance(addl, dict):
        _check_schema_keywords(addl, f"{path}.additionalProperties")


def resolve_schema_path(schema_name: str, schema_dir: Path = SCHEMA_DIR) -> Path:
    """Resolve a short schema name (e.g. 'event' or 'event.schema.json') to its file path."""
    cleaned = schema_name.removeprefix("v1/")
    if not cleaned.endswith(".schema.json"):
        cleaned = f"{cleaned}.schema.json"
    candidate = schema_dir / cleaned
    if not candidate.is_file():
        raise FileNotFoundError(f"W1 JSON Schema file not found: {candidate}")
    return candidate


def load_schema(schema_name: str, schema_dir: Path = SCHEMA_DIR) -> dict[str, Any]:
    """Load and parse a W1 JSON Schema file by name."""
    path = resolve_schema_path(schema_name, schema_dir=schema_dir)
    with path.open("r", encoding="utf-8") as fh:
        data = json.load(fh)
    if not isinstance(data, dict):
        raise SchemaValidationError(f"root schema in {path} must be a JSON object")
    _check_schema_keywords(data, "$")
    return data


def _resolve_pointer(pointer: str, root_schema: dict[str, Any], ref: str) -> dict[str, Any]:
    """Resolve a JSON Pointer fragment ('/...') against root_schema."""
    node: Any = root_schema
    for part in pointer.lstrip("/").split("/"):
        if not part:
            continue
        if not isinstance(node, dict) or part not in node:
            raise SchemaValidationError(f"unresolved $ref: {ref!r}")
        node = node[part]
    if not isinstance(node, dict):
        raise SchemaValidationError(f"target of $ref {ref!r} is not a schema object")
    return node


def _resolve_ref(
    ref: str,
    root_schema: dict[str, Any],
    schema_dir: Path = SCHEMA_DIR,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Resolve a local ('#/$defs/...') or sibling-file ('measurements.schema.json#...') $ref."""
    if ref.startswith("#"):
        target = _resolve_pointer(ref[1:], root_schema, ref)
        return target, root_schema
    file_part, has_frag, frag_part = ref.partition("#")
    if not file_part or "://" in file_part:
        raise SchemaValidationError(f"unsupported non-local $ref: {ref!r}")
    target_root = load_schema(file_part, schema_dir=schema_dir)
    if not has_frag or not frag_part:
        return target_root, target_root
    target = _resolve_pointer(frag_part, target_root, ref)
    return target, target_root


def _check_type(instance: Any, expected_type: str, path: str) -> None:
    """Validate JSON type matching JSON Schema primitive types."""
    if expected_type == "object":
        ok = isinstance(instance, dict)
    elif expected_type == "array":
        ok = isinstance(instance, list)
    elif expected_type == "string":
        ok = isinstance(instance, str)
    elif expected_type == "boolean":
        ok = isinstance(instance, bool)
    elif expected_type == "integer":
        ok = isinstance(instance, int) and not isinstance(instance, bool)
    elif expected_type == "number":
        ok = isinstance(instance, (int, float)) and not isinstance(instance, bool)
    elif expected_type == "null":
        ok = instance is None
    else:
        raise SchemaValidationError(f"{path}: unsupported schema type {expected_type!r}")
    if not ok:
        raise SchemaValidationError(
            f"{path}: expected type {expected_type}, got {type(instance).__name__}"
        )


def _validate_object(
    instance: dict[str, Any],
    schema: dict[str, Any],
    root_schema: dict[str, Any],
    path: str,
    schema_dir: Path = SCHEMA_DIR,
) -> None:
    """Validate object keywords (required, minProperties, properties, additionalProperties)."""
    for req_key in schema.get("required", []):
        if req_key not in instance:
            raise SchemaValidationError(f"{path}: missing required field {req_key!r}")
    min_props = schema.get("minProperties")
    if min_props is not None and len(instance) < min_props:
        raise SchemaValidationError(
            f"{path}: object has {len(instance)} properties, minimum is {min_props}"
        )
    props = schema.get("properties", {})
    addl = schema.get("additionalProperties", True)
    for key, val in instance.items():
        subpath = f"{path}.{key}"
        if key in props:
            _validate_node(val, props[key], root_schema, subpath, schema_dir)
        elif addl is False:
            raise SchemaValidationError(f'{path}: unknown field "{key}"')
        elif isinstance(addl, dict):
            _validate_node(val, addl, root_schema, subpath, schema_dir)


def _validate_constraints(instance: Any, schema: dict[str, Any], path: str) -> None:
    """Validate const, enum, minLength, and minimum constraints on an instance."""
    if "const" in schema and instance != schema["const"]:
        raise SchemaValidationError(
            f"{path}: value {instance!r} != const {schema['const']!r}"
        )
    if "enum" in schema and instance not in schema["enum"]:
        raise SchemaValidationError(
            f"{path}: value {instance!r} not in allowed enum {schema['enum']!r}"
        )
    if isinstance(instance, str):
        min_len = schema.get("minLength")
        if min_len is not None and len(instance) < min_len:
            raise SchemaValidationError(
                f"{path}: string length {len(instance)} < minLength {min_len}"
            )
    elif isinstance(instance, (int, float)) and not isinstance(instance, bool):
        minimum = schema.get("minimum")
        if minimum is not None and instance < minimum:
            raise SchemaValidationError(f"{path}: value {instance} < minimum {minimum}")


def _validate_node(
    instance: Any,
    schema: dict[str, Any],
    root_schema: dict[str, Any],
    path: str = "$",
    schema_dir: Path = SCHEMA_DIR,
) -> None:
    """Recursively validate a parsed JSON value against a schema node."""
    for key in schema:
        if key not in SUPPORTED_SCHEMA_KEYWORDS:
            raise SchemaValidationError(
                f"{path}: unsupported JSON Schema keyword {key!r}"
            )

    if "$ref" in schema:
        target, target_root = _resolve_ref(
            schema["$ref"], root_schema, schema_dir=schema_dir
        )
        _validate_node(instance, target, target_root, path, schema_dir)
        return

    expected_type = schema.get("type")
    if isinstance(expected_type, str):
        _check_type(instance, expected_type, path)

    _validate_constraints(instance, schema, path)

    if isinstance(instance, list):
        min_items = schema.get("minItems")
        if min_items is not None and len(instance) < min_items:
            raise SchemaValidationError(
                f"{path}: array length {len(instance)} < minItems {min_items}"
            )
        item_schema = schema.get("items")
        if isinstance(item_schema, dict):
            for idx, elem in enumerate(instance):
                _validate_node(
                    elem, item_schema, root_schema, f"{path}[{idx}]", schema_dir
                )
    elif isinstance(instance, dict):
        _validate_object(instance, schema, root_schema, path, schema_dir)


def validate_instance(
    instance: Any,
    schema_or_name: str | dict[str, Any],
    schema_dir: Path = SCHEMA_DIR,
) -> None:
    """Validate a single decoded Python object against a W1 JSON Schema."""
    if isinstance(schema_or_name, str):
        schema = load_schema(schema_or_name, schema_dir=schema_dir)
    else:
        schema = schema_or_name
        _check_schema_keywords(schema, "$")
    _validate_node(instance, schema, schema, "$", schema_dir=schema_dir)


def validate_file(
    file_path: str | Path,
    schema_name: str,
    schema_dir: Path = SCHEMA_DIR,
) -> int:
    """Validate a JSON or JSONL file against a W1 JSON Schema; returns record count."""
    schema = load_schema(schema_name, schema_dir=schema_dir)
    path = Path(file_path)
    text = path.read_text(encoding="utf-8")
    if path.suffix == ".jsonl":
        count = 0
        for line_no, raw_line in enumerate(text.splitlines(), start=1):
            stripped = raw_line.strip()
            if not stripped:
                continue
            try:
                record = json.loads(stripped)
            except json.JSONDecodeError as exc:
                raise SchemaValidationError(
                    f"{path}:{line_no}: invalid JSON: {exc}"
                ) from exc
            try:
                validate_instance(record, schema, schema_dir=schema_dir)
            except SchemaValidationError as exc:
                raise SchemaValidationError(f"{path}:{line_no}: {exc}") from exc
            count += 1
        return count

    try:
        doc = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SchemaValidationError(f"{path}: invalid JSON: {exc}") from exc
    validate_instance(doc, schema, schema_dir=schema_dir)
    return 1


def main(argv: list[str] | None = None) -> int:
    """CLI entrypoint for validating JSON/JSONL files against W1 schemas."""
    parser = argparse.ArgumentParser(
        description="Validate RL benchmark JSON/JSONL artifacts against W1 JSON Schemas."
    )
    parser.add_argument(
        "schema",
        help="Schema name (e.g. 'event', 'trace_event', 'report.schema.json')",
    )
    parser.add_argument(
        "files",
        nargs="+",
        help="One or more JSON or JSONL files to validate",
    )
    args = parser.parse_args(argv)
    for file_arg in args.files:
        try:
            validate_file(file_arg, args.schema)
        except (FileNotFoundError, SchemaValidationError) as exc:
            print(f"ERROR: {exc}", file=sys.stderr)
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
