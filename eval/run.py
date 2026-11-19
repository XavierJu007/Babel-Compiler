#!/usr/bin/env python3
"""Small Babel evaluation runner. Default mode validates fixture structure offline."""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

ANNOTATION_STATUS = "assistant_drafted_requires_human_review"
DEFAULT_CASES = Path(__file__).with_name("cases.jsonl")
ID_PATTERN = re.compile(r"[a-z0-9][a-z0-9-]*\Z")
MAX_RESPONSE_BYTES = 2 * 1024 * 1024
COMPARE_POLICY = "semantic-gate-v2"
BLIND_POLICY = "semantic-blind-v1"
BLIND_STAGE_POLICIES = {
    "local_checks": COMPARE_POLICY,
    "reader": "blind-reader-v1",
    "comparator": "blind-comparator-v1",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def is_text(value):
    return isinstance(value, str) and bool(value.strip())


def text_list(value, name, *, nonempty=False):
    require(isinstance(value, list), f"{name} must be a list")
    require(not nonempty or bool(value), f"{name} must not be empty")
    require(all(is_text(item) for item in value), f"{name} must contain nonempty strings")
    require(len(set(value)) == len(value), f"{name} contains duplicates")


def validate_case(case):
    require(isinstance(case, dict), "case must be an object")
    fields = {
        "schema_version", "id", "language", "domains", "tags", "annotation_status",
        "source", "valid_candidate", "expected_relations", "protected_literals",
        "invalid_candidates",
    }
    require(set(case) == fields, f"case fields differ: {sorted(set(case) ^ fields)}")
    require(type(case["schema_version"]) is int and case["schema_version"] == 1,
            "schema_version must be 1")
    require(is_text(case["id"]) and ID_PATTERN.fullmatch(case["id"]), "invalid case id")
    require(case["language"] in ("zh", "en"), "language must be zh or en")
    require(case["annotation_status"] == ANNOTATION_STATUS, "unsupported annotation status")
    text_list(case["domains"], "domains", nonempty=True)
    text_list(case["tags"], "tags", nonempty=True)
    require(is_text(case["source"]), "source must be nonempty")
    require(is_text(case["valid_candidate"]), "valid_candidate must be nonempty")
    text_list(case["protected_literals"], "protected_literals")
    for literal in case["protected_literals"]:
        require(literal in case["source"], f"protected literal missing from source: {literal!r}")
        require(literal in case["valid_candidate"],
                f"protected literal missing from valid_candidate: {literal!r}")
    relations = case["expected_relations"]
    require(isinstance(relations, list) and bool(relations), "expected_relations must be nonempty")
    relation_ids = set()
    for relation in relations:
        require(isinstance(relation, dict) and set(relation) == {"id", "statement"},
                "each relation needs exactly id and statement")
        require(is_text(relation["id"]) and relation["id"] not in relation_ids,
                "relation id must be nonempty and unique")
        require(is_text(relation["statement"]), "relation statement must be nonempty")
        relation_ids.add(relation["id"])
    mutants = case["invalid_candidates"]
    require(isinstance(mutants, list) and bool(mutants), "invalid_candidates must be nonempty")
    mutant_ids = set()
    for mutant in mutants:
        require(isinstance(mutant, dict) and set(mutant) ==
                {"id", "text", "violated_relations", "reason"}, "invalid mutant fields")
        require(is_text(mutant["id"]) and mutant["id"] not in mutant_ids,
                "mutant id must be nonempty and unique")
        mutant_ids.add(mutant["id"])
        require(is_text(mutant["text"]) and mutant["text"] not in
                (case["source"], case["valid_candidate"]), "mutant text must differ from source/candidate")
        require(is_text(mutant["reason"]), "mutant reason must be nonempty")
        text_list(mutant["violated_relations"], "violated_relations", nonempty=True)
        require(set(mutant["violated_relations"]) <= relation_ids,
                "mutant refers to an unknown relation")
    return case


def load_cases(path):
    raw = path.read_bytes()
    rows, ids = [], set()
    for number, line in enumerate(raw.decode("utf-8").splitlines(), 1):
        if not line.strip():
            continue
        try:
            case = validate_case(json.loads(line))
            require(case["id"] not in ids, "duplicate case id")
        except (ValueError, TypeError, KeyError) as exc:
            raise ValueError(f"{path.name}:{number}: {exc}") from exc
        rows.append(case)
        ids.add(case["id"])
    require(bool(rows), "case file is empty")
    return rows, hashlib.sha256(raw).hexdigest()


def validate_endpoint(endpoint):
    parsed = urllib.parse.urlsplit(endpoint.strip())
    require(parsed.scheme in ("http", "https") and bool(parsed.hostname),
            "--endpoint must be an explicit http(s) Babel base URL")
    require(parsed.username is None and parsed.password is None,
            "--endpoint must not contain credentials")
    require(not parsed.query and not parsed.fragment, "--endpoint must not contain query/fragment")
    return endpoint.strip().rstrip("/")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def request_json(url, payload, timeout):
    """Only called in explicit live mode; no config or credential endpoint is read."""
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST",
                                 headers={"Content-Type": "application/json"})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(req, timeout=timeout) as response:
            status, raw = response.status, response.read(MAX_RESPONSE_BYTES + 1)
    except urllib.error.HTTPError as exc:
        status, raw = exc.code, exc.read(MAX_RESPONSE_BYTES + 1)
        exc.close()
    except (urllib.error.URLError, TimeoutError, OSError):
        # Do not print upstream error bodies or connection details containing secrets.
        return None, {"response_error": "transport_error"}
    if len(raw) > MAX_RESPONSE_BYTES:
        return status, {"response_error": "response_too_large"}
    try:
        body = json.loads(raw)
    except (UnicodeDecodeError, ValueError):
        return status, {"response_error": "invalid_json"}
    if not isinstance(body, dict):
        return status, {"response_error": "json_object_required"}
    return status, body


def receipt_issues_valid(issues, status):
    if not isinstance(issues, list) or not all(is_text(issue) for issue in issues):
        return False
    return bool(issues) if status == "failed" else not issues


def reader_receipt_present(stage, *, rejected=False):
    # Check the service receipt, not semantic correctness of the reading.
    reading = stage.get("reading")
    return (
        isinstance(reading, dict)
        and type(reading.get("schema_version")) is int
        and reading["schema_version"] == 1
        and all(isinstance(reading.get(key), list)
                for key in ("entities", "relations", "ambiguities"))
        # A failed reader may report empty relations or unresolved ambiguities.
        # A completed reader must have relations and no unresolved ambiguity.
        and (rejected or (bool(reading["relations"]) and not reading["ambiguities"]))
    )


def recognized_validation(validation, expected_mode):
    """Recognize the requested protocol; a top-level passed flag is insufficient."""
    status = validation.get("status")
    if status == "error":
        return "error", "service_reported_error"
    if status not in ("passed", "failed"):
        return "unknown", "undecided_or_invalid_status"
    if expected_mode == "compare":
        if validation.get("policy_version") != COMPARE_POLICY:
            return "unknown", "compare_policy_mismatch"
        method = validation.get("method")
        if method not in ("llm_compare", "local_checks") or (
                status == "passed" and method != "llm_compare"):
            return "unknown", "compare_method_mismatch"
        if not receipt_issues_valid(validation.get("issues"), status):
            return "unknown", "inconsistent_issues"
        return status, "recognized_compare_receipt"
    if expected_mode != "blind":
        return "unknown", "unsupported_requested_mode"
    if (validation.get("method") != "blind_read"
            or validation.get("policy_version") != BLIND_POLICY):
        return "unknown", "blind_method_or_policy_mismatch"
    raw_stages = validation.get("stages")
    if not isinstance(raw_stages, list) or len(raw_stages) != len(BLIND_STAGE_POLICIES):
        return "unknown", "missing_blind_stages"
    stages = {}
    for stage in raw_stages:
        if not isinstance(stage, dict):
            return "unknown", "malformed_blind_stage"
        name = stage.get("name")
        if not isinstance(name, str) or name not in BLIND_STAGE_POLICIES or name in stages:
            return "unknown", "unknown_or_duplicate_blind_stage"
        if stage.get("policy_version") != BLIND_STAGE_POLICIES[name]:
            return "unknown", "blind_stage_policy_mismatch"
        stages[name] = stage
    if any(stage.get("status") == "error" for stage in stages.values()):
        return "error", "blind_stage_reported_error"
    if not receipt_issues_valid(validation.get("issues"), status):
        return "unknown", "inconsistent_issues"
    for stage in stages.values():
        stage_status = stage.get("status")
        if stage_status not in ("completed", "passed", "failed", "not_run"):
            return "unknown", "invalid_blind_stage_status"
        if not receipt_issues_valid(stage.get("issues"), stage_status):
            return "unknown", "inconsistent_blind_stage_issues"
    ordered = tuple(stages[name]["status"] for name in BLIND_STAGE_POLICIES)
    if status == "passed":
        if ordered != ("completed", "completed", "passed"):
            return "unknown", "blind_success_stages_incomplete"
        if not reader_receipt_present(stages["reader"]):
            return "unknown", "missing_or_ambiguous_reader_receipt"
        return "passed", "recognized_blind_receipt"
    if ordered == ("failed", "not_run", "not_run"):
        return "failed", "recognized_blind_local_rejection"
    if ordered == ("completed", "failed", "not_run"):
        if reader_receipt_present(stages["reader"], rejected=True):
            return "failed", "recognized_blind_reader_rejection"
        return "unknown", "missing_rejected_reader_receipt"
    if ordered == ("completed", "completed", "failed"):
        if reader_receipt_present(stages["reader"]):
            return "failed", "recognized_blind_comparator_rejection"
        return "unknown", "missing_or_ambiguous_reader_receipt"
    return "unknown", "inconsistent_blind_rejection_stages"


def validation_observation(http_status, body, expected_mode="compare"):
    validation = body.get("validation")
    if http_status != 200:
        category, receipt_check = "error", "transport_or_http_error"
    elif not isinstance(validation, dict):
        category, receipt_check = "unknown", "missing_validation_object"
    else:
        category, receipt_check = recognized_validation(validation, expected_mode)
    detail = {"requested_mode": expected_mode, "receipt_check": receipt_check}
    if isinstance(validation, dict):
        for key in ("status", "method", "policy_version"):
            if isinstance(validation.get(key), str):
                detail[key] = validation[key]
        # Error messages can contain raw provider responses; never copy those.
        if category in ("passed", "failed"):
            detail["issues"] = validation["issues"]
        elif validation.get("issues"):
            detail["issues_omitted"] = True
        if isinstance(validation.get("stages"), list):
            detail["stages"] = []
            for stage in validation["stages"]:
                if not isinstance(stage, dict):
                    continue
                safe = {key: stage[key] for key in ("name", "status", "policy_version", "model")
                        if isinstance(stage.get(key), str)}
                if type(stage.get("duration_ms")) is int and stage["duration_ms"] >= 0:
                    safe["duration_ms"] = stage["duration_ms"]
                if category in ("passed", "failed"):
                    issues = stage.get("issues")
                    if isinstance(issues, list) and all(is_text(issue) for issue in issues):
                        safe["issues"] = issues
                    if isinstance(stage.get("reading"), dict):
                        safe["reading"] = stage["reading"]
                elif stage.get("issues"):
                    safe["issues_omitted"] = True
                detail["stages"].append(safe)
    return category, detail


def make_plan(cases, operation):
    plan = []
    for case in cases:
        if operation == "compile":
            plan.append((case, "generated", None, None))
        else:
            plan.append((case, "valid", case["valid_candidate"], "passed"))
            for mutant in case["invalid_candidates"]:
                plan.append((case, mutant["id"], mutant["text"], "failed"))
    return plan


def emit_record(handle, record):
    handle.write(json.dumps(record, ensure_ascii=False, sort_keys=True) + "\n")
    handle.flush()


def run_live(args, cases, dataset_sha, handle):
    plan = make_plan(cases, args.live)
    emit_record(handle, {
        "record_type": "manifest", "format_version": 1,
        "started_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
        "annotation_status": ANNOTATION_STATUS, "dataset_sha256": dataset_sha,
        "operation": args.live, "endpoint": args.endpoint, "profile": args.profile,
        "reader_profile": args.reader_profile, "validation_mode": args.validation_mode,
        "expected_validation_policy": (
            BLIND_POLICY if args.validation_mode == "blind" else COMPARE_POLICY
        ) if args.live == "validate" else None,
        "output_preset": args.mode, "token_encoding_requested": args.token_encoding,
        "planned_requests": len(plan), "max_requests": args.max_requests,
        "selected_case_ids": [case["id"] for case in cases],
        "upstream_model_identity": "not_independently_verified",
        "evidence_scope": "Babel API observations; HTTP budget is not an upstream-model-call budget",
    })
    matrix = {"draft_valid": {"passed": 0, "failed": 0},
              "draft_invalid": {"passed": 0, "failed": 0}}
    counts = {"requests": 0, "api_success": 0, "error": 0, "unknown": 0}
    for case, candidate_id, candidate, expected in plan[:args.max_requests]:
        payload = {"text": case["source"], "profile": args.profile}
        if args.live == "compile":
            payload.update(mode=args.mode, domains=case["domains"], verify=False,
                           strict_gate=False, no_cache=True)
            if args.token_encoding:
                payload["token_encoding"] = args.token_encoding
            route = "/api/compile"
        else:
            payload.update(candidate=candidate, validation_mode=args.validation_mode)
            if args.reader_profile is not None:
                payload["reader_profile"] = args.reader_profile
            route = "/api/validate"
        start = time.monotonic()
        http_status, body = request_json(args.endpoint + route, payload, args.timeout)
        counts["requests"] += 1
        record = {
            "record_type": "observation", "case_id": case["id"],
            "candidate_id": candidate_id, "http_status": http_status,
            "elapsed_seconds": round(time.monotonic() - start, 6),
            "source": case["source"], "expected_relations": case["expected_relations"],
            "protected_literals": case["protected_literals"],
        }
        if args.live == "compile":
            compiled = body.get("compiled")
            success = http_status == 200 and is_text(compiled)
            category = "api_success" if success else ("error" if http_status != 200 else "unknown")
            record["result"] = category
            if success:
                record["candidate"] = compiled
                record["literal_presence_check"] = {
                    "missing": [literal for literal in case["protected_literals"] if literal not in compiled],
                    "semantic_equivalence": "not_evaluated",
                }
                # Preserve measurement kind/source/encoding/scope rather than
                # turning estimated text lengths into a billing or semantic score.
                if isinstance(body.get("stats"), dict):
                    record["stats"] = body["stats"]
                if isinstance(body.get("usage"), dict):
                    record["usage"] = body["usage"]
                _, validation = validation_observation(http_status, body)
                record["validation"] = validation
            record["semantic_quality"] = "not_evaluated"
        else:
            category, validation = validation_observation(http_status, body, args.validation_mode)
            record.update(candidate=candidate, draft_expected_validation=expected,
                          result=category, validation=validation)
            if category in ("passed", "failed"):
                row = "draft_valid" if expected == "passed" else "draft_invalid"
                matrix[row][category] += 1
                counts["api_success"] += 1
        if category in counts:
            counts[category] += 1
        emit_record(handle, record)
    summary = {
        "record_type": "summary", "operation": args.live, **counts,
        "planned_requests": len(plan), "skipped_by_budget": max(0, len(plan) - counts["requests"]),
        "annotation_status": ANNOTATION_STATUS,
        "semantic_accuracy": "not_established",
        "upstream_model_identity": "not_independently_verified",
    }
    if args.live == "validate":
        decided = sum(sum(row.values()) for row in matrix.values())
        agreement = matrix["draft_valid"]["passed"] + matrix["draft_invalid"]["failed"]
        summary.update(
            draft_label_confusion_matrix=matrix,
            draft_label_agreement=(agreement / decided if decided else None),
            draft_label_agreement_denominator=decided,
            excluded_from_agreement={"error": counts["error"], "unknown": counts["unknown"]},
        )
    emit_record(handle, summary)
    return summary


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cases", type=Path, default=DEFAULT_CASES)
    parser.add_argument("--live", choices=("compile", "validate"),
                        help="explicitly send requests; omitted means offline structure checks only")
    parser.add_argument("--endpoint", help="Babel base URL; required for live mode")
    parser.add_argument("--profile", help="explicit saved Babel profile; required for live mode")
    parser.add_argument("--reader-profile",
                        help="blind reader profile: omitted inherits; empty clears the saved override")
    parser.add_argument("--validation-mode", choices=("compare", "blind"), default="compare")
    parser.add_argument("--mode", default="standard", help="output preset for live compilation")
    parser.add_argument("--token-encoding", choices=("estimate", "cl100k_base", "o200k_base"))
    parser.add_argument("--case-id", action="append", default=[], help="case ID to include; repeatable")
    parser.add_argument("--limit", type=int, default=2, help="live case limit; default 2, in file order")
    parser.add_argument("--max-requests", type=int, help="explicit hard HTTP request budget; no retries")
    parser.add_argument("--timeout", type=float, default=60, help="per HTTP request timeout in seconds")
    parser.add_argument("--output", type=Path, help="new JSONL result file; required for live mode")
    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        cases, dataset_sha = load_cases(args.cases)
        require(args.limit > 0 and args.timeout > 0, "--limit and --timeout must be positive")
        selected_ids = set(args.case_id)
        require(selected_ids <= {case["id"] for case in cases}, "--case-id contains an unknown ID")
        if not args.live:
            require(not args.endpoint and not args.profile and args.reader_profile is None
                    and args.max_requests is None and args.token_encoding is None,
                    "network/model options require explicit --live")
            summary = {
                "operation": "offline_structure_check", "structure_checks": "passed",
                "cases": len(cases), "invalid_candidates": sum(len(c["invalid_candidates"]) for c in cases),
                "dataset_sha256": dataset_sha, "annotation_status": ANNOTATION_STATUS,
                "network_requests": 0, "semantic_quality": "not_evaluated",
            }
            if args.output:
                args.output.parent.mkdir(parents=True, exist_ok=True)
                with args.output.open("x", encoding="utf-8") as handle:
                    emit_record(handle, summary)
            print(json.dumps(summary, ensure_ascii=False, sort_keys=True))
            return 0
        require(args.endpoint and args.profile and args.profile.strip(),
                "--live requires explicit --endpoint and --profile")
        require(args.max_requests is not None and args.max_requests > 0,
                "--live requires a positive explicit --max-requests")
        require(args.output is not None, "--live requires --output for reviewable records")
        require(args.reader_profile is None or
                (args.live == "validate" and args.validation_mode == "blind"),
                "--reader-profile is only used with --live validate --validation-mode blind")
        require(not args.token_encoding or args.live == "compile",
                "--token-encoding is only used with --live compile")
        args.endpoint = validate_endpoint(args.endpoint)
        selected = [case for case in cases if not selected_ids or case["id"] in selected_ids][:args.limit]
        args.output.parent.mkdir(parents=True, exist_ok=True)
        with args.output.open("x", encoding="utf-8") as handle:
            summary = run_live(args, selected, dataset_sha, handle)
        print(json.dumps(summary, ensure_ascii=False, sort_keys=True))
        return 2 if summary["error"] or summary["unknown"] else 0
    except (ValueError, OSError, UnicodeDecodeError) as exc:
        print(f"evaluation error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

