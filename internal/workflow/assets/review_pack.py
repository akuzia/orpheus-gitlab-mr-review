#!/usr/bin/env python3
"""Validate review artifacts and emit one deterministic compressed envelope."""

import base64
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import sys
import zlib

SCHEMA_VERSION = 1
MAX_CONTRACT_BYTES = 64 * 1024
MAX_ARTIFACT_BYTES = 256 * 1024
MAX_FIELD_BYTES = 4 * 1024
MAX_ITEMS = 1000
MAX_PAYLOAD_BYTES = 4 * 1024 * 1024
MAX_ENVELOPE_BYTES = 480 * 1024
FINDING_NAME = re.compile(r"^F-[0-9]{4,}\.md$")
SEVERITIES = {"info", "warning", "error"}


class InvalidArtifacts(Exception):
    pass


def fail(message):
    print(f"review-pack: {message}", file=sys.stderr)
    return 2


def read_text(path, limit=MAX_ARTIFACT_BYTES):
    if path.is_symlink() or not path.is_file():
        raise InvalidArtifacts(f"artifact is not a regular file: {path.name}")
    data = path.read_bytes()
    if len(data) > limit:
        raise InvalidArtifacts(f"artifact exceeds size limit: {path.name}")
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as error:
        raise InvalidArtifacts(f"artifact is not UTF-8: {path.name}") from error
    if "\x00" in text:
        raise InvalidArtifacts(f"artifact contains NUL: {path.name}")
    return text.replace("\r\n", "\n").replace("\r", "\n")


def nonempty(value, field):
    if not isinstance(value, str) or not value.strip():
        raise InvalidArtifacts(f"{field} must be non-empty")
    if len(value.encode("utf-8")) > MAX_FIELD_BYTES:
        raise InvalidArtifacts(f"{field} exceeds size limit")
    return value


def parse_scalar(value):
    value = value.strip()
    if value.startswith('"'):
        try:
            parsed = json.loads(value)
        except json.JSONDecodeError as error:
            raise InvalidArtifacts("invalid quoted front matter value") from error
        if not isinstance(parsed, str):
            raise InvalidArtifacts("front matter value must be a string")
        return parsed
    return value


def parse_markdown(path, required, optional=None):
    text = read_text(path)
    lines = text.split("\n")
    if not lines or lines[0] != "---":
        raise InvalidArtifacts(f"missing front matter: {path.name}")
    try:
        closing = lines.index("---", 1)
    except ValueError as error:
        raise InvalidArtifacts(f"unterminated front matter: {path.name}") from error
    fields = {}
    allowed = required | (optional or set())
    for line in lines[1:closing]:
        if not line.strip() or ":" not in line:
            raise InvalidArtifacts(f"invalid front matter line: {path.name}")
        key, value = line.split(":", 1)
        key = key.strip()
        if key in fields:
            raise InvalidArtifacts(f"duplicate front matter field: {key}")
        if key not in allowed:
            raise InvalidArtifacts(f"unknown front matter field: {key}")
        fields[key] = parse_scalar(value)
    missing = set(required) - set(fields)
    if missing:
        raise InvalidArtifacts(f"missing front matter field: {sorted(missing)[0]}")
    body = "\n".join(lines[closing + 1 :]).strip()
    if not body:
        raise InvalidArtifacts(f"empty artifact body: {path.name}")
    if len(body.encode("utf-8")) > MAX_ARTIFACT_BYTES:
        raise InvalidArtifacts(f"artifact body exceeds size limit: {path.name}")
    return fields, body + "\n"


def list_files(directory):
    if directory.is_symlink() or not directory.is_dir():
        raise InvalidArtifacts(f"missing artifact directory: {directory.name}")
    entries = sorted(directory.iterdir(), key=lambda item: item.name)
    if len(entries) > MAX_ITEMS:
        raise InvalidArtifacts(f"too many artifacts in: {directory.name}")
    for entry in entries:
        if entry.is_symlink() or not entry.is_file():
            raise InvalidArtifacts(f"unexpected artifact entry: {entry.name}")
    return entries


def parse_int(value, field):
    try:
        parsed = int(value)
    except (TypeError, ValueError) as error:
        raise InvalidArtifacts(f"{field} must be an integer") from error
    if parsed <= 0:
        raise InvalidArtifacts(f"{field} must be positive")
    return parsed


def clean_repository_path(value):
    value = nonempty(value, "path")
    candidate = PurePosixPath(value)
    if candidate.is_absolute() or value == "." or ".." in candidate.parts or "\\" in value:
        raise InvalidArtifacts("path must be repository-relative and clean")
    if str(candidate) != value:
        raise InvalidArtifacts("path must be repository-relative and clean")
    return value


def parse_findings(directory):
    findings = []
    required = {"id", "path", "line", "severity", "title", "source"}
    previous_fields = {
        "previous_discussion_id",
        "previous_note_id",
        "previous_marker",
        "recurrence_comment",
    }
    for artifact in list_files(directory):
        if not FINDING_NAME.fullmatch(artifact.name):
            raise InvalidArtifacts(f"invalid finding filename: {artifact.name}")
        fields, body = parse_markdown(artifact, required, previous_fields)
        finding_id = nonempty(fields["id"], "id")
        if artifact.stem != finding_id:
            raise InvalidArtifacts(f"finding filename does not match id: {artifact.name}")
        severity = nonempty(fields["severity"], "severity")
        if severity not in SEVERITIES:
            raise InvalidArtifacts(f"invalid finding severity: {artifact.name}")
        finding = {
            "id": finding_id,
            "path": clean_repository_path(fields["path"]),
            "line": parse_int(fields["line"], "line"),
            "severity": severity,
            "title": nonempty(fields["title"], "title"),
            "source": nonempty(fields["source"], "source"),
            "body": body,
        }
        supplied_previous = previous_fields.intersection(fields)
        if supplied_previous and supplied_previous != previous_fields:
            raise InvalidArtifacts(f"incomplete previous finding reference: {artifact.name}")
        if supplied_previous:
            finding["previous"] = {
                "discussion_id": nonempty(fields["previous_discussion_id"], "previous_discussion_id"),
                "note_id": parse_int(fields["previous_note_id"], "previous_note_id"),
                "marker": nonempty(fields["previous_marker"], "previous_marker"),
                "recurrence_comment": nonempty(fields["recurrence_comment"], "recurrence_comment"),
            }
        findings.append(finding)
    return findings


def parse_recommendations(directory):
    recommendations = []
    for artifact in list_files(directory):
        if artifact.suffix != ".md" or artifact.name in {".", ".."}:
            raise InvalidArtifacts(f"invalid recommendation filename: {artifact.name}")
        body = read_text(artifact).strip()
        if not body:
            raise InvalidArtifacts(f"empty recommendation: {artifact.name}")
        recommendations.append({"name": artifact.name, "body": body + "\n"})
    return recommendations


def parse_resolutions(directory):
    resolutions = []
    required = {"discussion_id", "note_id", "marker"}
    for artifact in list_files(directory):
        if artifact.suffix != ".md":
            raise InvalidArtifacts(f"invalid resolution filename: {artifact.name}")
        fields, body = parse_markdown(artifact, required)
        resolutions.append(
            {
                "discussion_id": nonempty(fields["discussion_id"], "discussion_id"),
                "note_id": parse_int(fields["note_id"], "note_id"),
                "marker": nonempty(fields["marker"], "marker"),
                "body": body,
            }
        )
    resolutions.sort(key=lambda item: (item["discussion_id"], item["note_id"]))
    return resolutions


def expect_mapping(value, field):
    if not isinstance(value, dict):
        raise InvalidArtifacts(f"contract field must be an object: {field}")
    return value


def contract_value(mapping, name, expected_type):
    value = mapping.get(name)
    if not isinstance(value, expected_type) or isinstance(value, bool):
        raise InvalidArtifacts(f"invalid contract field: {name}")
    if expected_type is str:
        return nonempty(value, name)
    if expected_type is int and value <= 0:
        raise InvalidArtifacts(f"contract field must be positive: {name}")
    return value


def read_contract(review_dir):
    contract_path = review_dir / "contract.json"
    text = read_text(contract_path, MAX_CONTRACT_BYTES)
    try:
        contract = json.loads(text)
    except json.JSONDecodeError as error:
        raise InvalidArtifacts("invalid contract JSON") from error
    if not isinstance(contract, dict) or contract.get("schema_version") != SCHEMA_VERSION:
        raise InvalidArtifacts("unsupported contract schema")
    gitlab = expect_mapping(contract.get("gitlab"), "gitlab")
    review = expect_mapping(contract.get("review"), "review")
    protocol = expect_mapping(contract.get("protocol"), "protocol")
    if protocol.get("bundle_schema_version") != SCHEMA_VERSION:
        raise InvalidArtifacts("unsupported bundle schema")
    return {
        "workflow": {
            "id": contract_value(contract, "workflow_id", str),
            "revision": contract_value(contract, "workflow_revision", str),
            "helper_sha256": contract_value(protocol, "helper_sha256", str),
        },
        "identity": {
            "gitlab_host": contract_value(gitlab, "host", str),
            "project_id": contract_value(gitlab, "project_id", int),
            "mr_iid": contract_value(gitlab, "mr_iid", int),
            "reviewer_user_id": contract_value(gitlab, "reviewer_user_id", int),
        },
        "review": {
            "diff_fingerprint": contract_value(review, "diff_fingerprint", str),
            "review_fingerprint": contract_value(review, "review_fingerprint", str),
        },
    }


def build_bundle(review_dir):
    contract = read_contract(review_dir)
    pending = list_files(review_dir / "findings")
    if pending:
        raise InvalidArtifacts("pending findings must be empty")
    confirmed = parse_findings(review_dir / "confirmed")
    rejected = parse_findings(review_dir / "rejected")
    identifiers = [item["id"] for item in confirmed + rejected]
    if len(identifiers) != len(set(identifiers)):
        raise InvalidArtifacts("finding ids must be unique")
    recommendations = parse_recommendations(review_dir / "recommendations")
    resolutions = parse_resolutions(review_dir / "resolutions")
    if len(identifiers) + len(recommendations) + len(resolutions) > MAX_ITEMS:
        raise InvalidArtifacts("artifact count exceeds limit")
    return {
        "schema_version": SCHEMA_VERSION,
        "stage": "ready",
        "workflow": contract["workflow"],
        "identity": contract["identity"],
        "review": contract["review"],
        "counts": {
            "confirmed": len(confirmed),
            "rejected": len(rejected),
            "recommendations": len(recommendations),
            "resolutions": len(resolutions),
            "pending": 0,
        },
        "confirmed": confirmed,
        "recommendations": recommendations,
        "resolutions": resolutions,
    }


def encode(bundle):
    payload = json.dumps(bundle, ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode("utf-8")
    if len(payload) > MAX_PAYLOAD_BYTES:
        raise InvalidArtifacts("review_bundle_too_large: payload exceeds size limit")
    envelope = {
        "schema_version": SCHEMA_VERSION,
        "encoding": "zlib+base64",
        "payload_sha256": "sha256:" + hashlib.sha256(payload).hexdigest(),
        "uncompressed_bytes": len(payload),
        "payload": base64.b64encode(zlib.compress(payload, 9)).decode("ascii"),
    }
    output = json.dumps(envelope, separators=(",", ":"), sort_keys=True).encode("utf-8")
    if len(output) > MAX_ENVELOPE_BYTES:
        raise InvalidArtifacts("review_bundle_too_large: envelope exceeds size limit")
    return output


def main():
    if len(sys.argv) != 2:
        return fail("expected exactly one review directory argument")
    try:
        review_dir = Path(sys.argv[1])
        output = encode(build_bundle(review_dir))
        os.write(sys.stdout.fileno(), output)
        return 0
    except (InvalidArtifacts, OSError) as error:
        return fail(str(error))


if __name__ == "__main__":
    sys.exit(main())
