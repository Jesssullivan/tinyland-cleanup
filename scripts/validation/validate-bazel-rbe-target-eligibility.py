#!/usr/bin/env python3
"""Validate the tinyland-cleanup Bazel RBE target eligibility manifest."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any


REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_MANIFEST = REPO_ROOT / "config" / "bazel-rbe-target-eligibility.json"


def load_manifest(path: Path) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            data = json.load(handle)
    except FileNotFoundError:
        raise SystemExit(f"error: manifest not found: {path}") from None
    except json.JSONDecodeError as exc:
        raise SystemExit(f"error: invalid JSON in {path}: {exc}") from None

    if not isinstance(data, dict):
        raise SystemExit("error: manifest must be a JSON object")
    return data


def require(condition: bool, message: str, errors: list[str]) -> None:
    if not condition:
        errors.append(message)


def ensure_string_list(value: Any, field: str, errors: list[str], *, nonempty: bool = True) -> list[str]:
    if not isinstance(value, list) or not all(isinstance(item, str) and item for item in value):
        errors.append(f"{field} must be a list of nonempty strings")
        return []
    if nonempty and not value:
        errors.append(f"{field} must not be empty")
    return value


def validate_entry_list(
    data: dict[str, Any],
    field: str,
    errors: list[str],
    *,
    require_proof: bool,
) -> list[dict[str, Any]]:
    entries = data.get(field)
    if not isinstance(entries, list):
        errors.append(f"{field} must be a list")
        return []

    typed_entries: list[dict[str, Any]] = []
    for index, entry in enumerate(entries):
        prefix = f"{field}[{index}]"
        if not isinstance(entry, dict):
            errors.append(f"{prefix} must be an object")
            continue

        typed_entries.append(entry)
        require(isinstance(entry.get("name"), str) and bool(entry.get("name")), f"{prefix}.name is required", errors)
        labels = ensure_string_list(entry.get("labels"), f"{prefix}.labels", errors)

        if field != "blocked_target_classes":
            for label in labels:
                if label.startswith("//") and ":" not in label:
                    errors.append(f"{prefix}.labels entry {label!r} must use an explicit Bazel target label")

            status = entry.get("status")
            require(status in {"candidate", "proved"}, f"{prefix}.status must be candidate or proved", errors)
            ensure_string_list(entry.get("allowed_modes"), f"{prefix}.allowed_modes", errors)
            ensure_string_list(entry.get("requirements"), f"{prefix}.requirements", errors)
            proof_required = ensure_string_list(entry.get("proof_required"), f"{prefix}.proof_required", errors)
            require(
                any("remote" in item.lower() and "process" in item.lower() for item in proof_required),
                f"{prefix}.proof_required must require remote executor process evidence",
                errors,
            )
            if require_proof:
                require(status == "proved", f"{prefix} must be proved before entering proved_target_classes", errors)
        else:
            ensure_string_list(entry.get("blockers"), f"{prefix}.blockers", errors)

    return typed_entries


def validate_manifest(data: dict[str, Any]) -> tuple[list[dict[str, Any]], list[dict[str, Any]], list[dict[str, Any]]]:
    errors: list[str] = []

    require(data.get("schema_version") == 1, "schema_version must be 1", errors)
    owner_issue = data.get("owner_issue")
    require(isinstance(owner_issue, str) and bool(owner_issue), "owner_issue is required", errors)
    require(isinstance(data.get("updated"), str) and bool(data.get("updated")), "updated is required", errors)
    require(isinstance(data.get("platform"), str) and bool(data.get("platform")), "platform is required", errors)
    default_posture = data.get("default_posture")
    require(
        isinstance(default_posture, str) and "cache-forward" in default_posture,
        "default_posture must describe cache-forward posture",
        errors,
    )

    proved = validate_entry_list(data, "proved_target_classes", errors, require_proof=True)
    candidates = validate_entry_list(data, "candidate_target_classes", errors, require_proof=False)
    blocked = validate_entry_list(data, "blocked_target_classes", errors, require_proof=False)

    invariants = ensure_string_list(data.get("invariants"), "invariants", errors)
    invariant_text = "\n".join(invariants).lower()
    for required in ("remote cache hits", "rbe proof", "attic", "flakehub", "broad presubmit", "runner"):
        require(required in invariant_text, f"invariants must mention {required!r}", errors)

    require(bool(candidates), "candidate_target_classes must contain at least one explicit proof candidate", errors)
    require(bool(blocked), "blocked_target_classes must document known unsafe target classes", errors)

    seen_labels: dict[str, str] = {}
    for section, entries in (
        ("proved_target_classes", proved),
        ("candidate_target_classes", candidates),
        ("blocked_target_classes", blocked),
    ):
        for entry in entries:
            for label in entry.get("labels", []):
                previous = seen_labels.setdefault(label, section)
                if previous != section:
                    errors.append(f"label {label!r} appears in both {previous} and {section}")

    if errors:
        print("Bazel RBE target eligibility manifest failed:", file=sys.stderr)
        for error in errors:
            print(f"  - {error}", file=sys.stderr)
        raise SystemExit(1)

    return proved, candidates, blocked


def find_target(target: str, entries: list[dict[str, Any]]) -> dict[str, Any] | None:
    for entry in entries:
        if target in entry.get("labels", []):
            return entry
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, default=DEFAULT_MANIFEST)
    parser.add_argument("--target", help="Require that a target is eligible for explicit proof")
    parser.add_argument("--allow-candidate", action="store_true", help="Allow candidate targets for proof runs")
    parser.add_argument("--format", choices=("text", "json"), default="text")
    args = parser.parse_args()

    data = load_manifest(args.manifest)
    proved, candidates, blocked = validate_manifest(data)

    result: dict[str, Any] = {
        "manifest": str(args.manifest),
        "proved_target_classes": len(proved),
        "candidate_target_classes": len(candidates),
        "blocked_target_classes": len(blocked),
    }

    if args.target:
        target = args.target
        if find_target(target, blocked):
            print(f"error: target {target} is explicitly blocked from RBE proof", file=sys.stderr)
            return 1
        if find_target(target, proved):
            result["target_status"] = "proved"
        elif find_target(target, candidates):
            if not args.allow_candidate:
                print(f"error: target {target} is a candidate; pass --allow-candidate for proof runs", file=sys.stderr)
                return 1
            result["target_status"] = "candidate"
        else:
            print(f"error: target {target} is not listed for RBE proof", file=sys.stderr)
            return 1

    if args.format == "json":
        print(json.dumps(result, indent=2, sort_keys=True))
    else:
        print("Bazel RBE target eligibility manifest passed")
        if args.target:
            print(f"Target {args.target}: {result['target_status']}")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
