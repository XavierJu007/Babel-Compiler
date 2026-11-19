import contextlib
import copy
import importlib.util
import io
import json
import tempfile
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("babel_eval_runner", Path(__file__).with_name("run.py"))
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


def validation(status, issues=None):
    if issues is None:
        issues = ["fixture rejection"] if status == "failed" else []
    return {"validation": {"status": status, "method": "llm_compare",
                           "policy_version": runner.COMPARE_POLICY, "issues": issues}}


def blind_validation():
    """A current-protocol mock receipt; this is not a real model result."""
    stages = [
        {"name": name, "policy_version": policy, "issues": [],
         "status": "passed" if name == "comparator" else "completed"}
        for name, policy in runner.BLIND_STAGE_POLICIES.items()
    ]
    stages[1].update(model="fixture-mock", reading={
        "schema_version": 1, "entities": ["cache"], "ambiguities": [],
        "relations": [{"subject": "cache", "relation": "is", "object": "empty",
                       "conditions": [], "scope": "", "modality": "fact", "negated": False,
                       "quantities": [], "evidence": "cache is empty"}],
    })
    stages[2]["model"] = "fixture-mock"
    return {"validation": {"status": "passed", "method": "blind_read",
                           "policy_version": runner.BLIND_POLICY, "issues": [], "stages": stages}}


def blind_rejection(stage_name):
    body = blind_validation()
    result = body["validation"]
    result.update(status="failed", issues=["fixture rejection"])
    stopped = False
    for stage in result["stages"]:
        if stage["name"] == stage_name:
            stage.update(status="failed", issues=["fixture rejection"])
            stopped = True
        elif stopped:
            stage["status"] = "not_run"
            stage.pop("reading", None)
    return body


class EvaluationRunnerTests(unittest.TestCase):
    def invoke(self, args):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = runner.main(args)
        return result, stdout.getvalue(), stderr.getvalue()

    def live_args(self, path, operation="validate"):
        return ["--live", operation, "--endpoint", "http://127.0.0.1:8080",
                "--profile", "explicit-test-profile", "--max-requests", "1",
                "--output", str(path)]

    def test_default_offline_never_calls_api(self):
        with patch.object(runner, "request_json", side_effect=AssertionError("offline used network")):
            code, output, _ = self.invoke([])
        self.assertEqual(code, 0)
        summary = json.loads(output)
        self.assertEqual(summary["cases"], 30)
        self.assertEqual(summary["invalid_candidates"], 60)
        self.assertEqual(summary["network_requests"], 0)
        self.assertEqual(summary["semantic_quality"], "not_evaluated")

    def test_schema_rejects_unknown_relation_and_damaged_literal(self):
        cases, _ = runner.load_cases(runner.DEFAULT_CASES)
        damaged = copy.deepcopy(cases[0])
        damaged["invalid_candidates"][0]["violated_relations"] = ["not-in-case"]
        with self.assertRaisesRegex(ValueError, "unknown relation"):
            runner.validate_case(damaged)
        literal_case = copy.deepcopy(next(case for case in cases if case["id"] == "zh-command-spacing"))
        literal_case["valid_candidate"] = literal_case["valid_candidate"].replace("a  b", "a b")
        with self.assertRaisesRegex(ValueError, "protected literal"):
            runner.validate_case(literal_case)

    def test_live_requires_explicit_profile_and_budget(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.live_args(Path(tmp) / "run.jsonl")
            index = args.index("--profile")
            del args[index:index + 2]
            with patch.object(runner, "request_json") as request:
                code, _, error = self.invoke(args)
            self.assertEqual(code, 1)
            self.assertIn("--profile", error)
            request.assert_not_called()
            args = self.live_args(Path(tmp) / "run.jsonl")
            index = args.index("--max-requests")
            del args[index:index + 2]
            with patch.object(runner, "request_json") as request:
                code, _, error = self.invoke(args)
            self.assertEqual(code, 1)
            self.assertIn("--max-requests", error)
            request.assert_not_called()

    def test_budget_and_draft_label_matrix_exclude_errors_and_unknowns(self):
        responses = [
            (200, validation("passed")),
            (200, validation("failed", ["changed condition"])),
            (502, validation("error", ["provider details must not be recorded"])),
            (200, validation("not_run")),
            (200, validation("passed")),
        ]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "run.jsonl"
            args = self.live_args(path)
            args[args.index("--max-requests") + 1] = "5"
            with patch.object(runner, "request_json", side_effect=responses) as request:
                code, output, _ = self.invoke(args)
            self.assertEqual(request.call_count, 5)
            self.assertEqual(code, 2)
            summary = json.loads(output)
            self.assertEqual(summary["planned_requests"], 6)
            self.assertEqual(summary["skipped_by_budget"], 1)
            self.assertEqual(summary["api_success"], 3)
            self.assertEqual(summary["draft_label_agreement_denominator"], 3)
            self.assertAlmostEqual(summary["draft_label_agreement"], 2 / 3)
            self.assertEqual(summary["draft_label_confusion_matrix"],
                             {"draft_valid": {"passed": 1, "failed": 0},
                              "draft_invalid": {"passed": 1, "failed": 1}})
            self.assertEqual(summary["excluded_from_agreement"], {"error": 1, "unknown": 1})
            records = [json.loads(line) for line in path.read_text().splitlines()]
            self.assertEqual(len(records), 7)  # manifest, five observations, summary
            self.assertNotIn("provider details", path.read_text())
            self.assertEqual(summary["semantic_accuracy"], "not_established")
            for call in request.call_args_list:
                self.assertTrue(call.args[0].endswith("/api/validate"))
                payload = call.args[1]
                self.assertEqual(payload["profile"], "explicit-test-profile")
                self.assertNotIn("expected_relations", payload)
                self.assertNotIn("api_key", payload)

    def test_compile_preserves_measurement_metadata_without_scoring_semantics(self):
        cases, _ = runner.load_cases(runner.DEFAULT_CASES)
        stats = {"original_tokens": 30, "compiled_tokens": 24,
                 "measurement_kind": "exact", "measurement_source": "fixture_encoding",
                 "measurement_scope": "text", "measurement_encoding": "cl100k_base",
                 "measurement_version": "fixture"}
        response = {"compiled": cases[0]["valid_candidate"], "stats": stats,
                    "validation": {"status": "not_run", "method": "none", "policy_version": "fixture"}}
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "compile.jsonl"
            args = self.live_args(path, "compile") + ["--token-encoding", "cl100k_base"]
            with patch.object(runner, "request_json", return_value=(200, response)) as request:
                code, output, _ = self.invoke(args)
            self.assertEqual(code, 0)
            payload = request.call_args.args[1]
            self.assertEqual(payload["token_encoding"], "cl100k_base")
            self.assertIs(payload["verify"], False)
            self.assertIs(payload["no_cache"], True)
            record = json.loads(path.read_text().splitlines()[1])
            self.assertEqual(record["stats"], stats)
            self.assertEqual(record["literal_presence_check"]["semantic_equivalence"], "not_evaluated")
            self.assertEqual(record["semantic_quality"], "not_evaluated")
            self.assertNotIn("draft_label_agreement", json.loads(output))

    def test_blind_request_contains_only_source_candidate_and_selected_profiles(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = self.live_args(Path(tmp) / "blind.jsonl") + [
                "--validation-mode", "blind", "--reader-profile", "explicit-reader"]
            with patch.object(runner, "request_json", return_value=(200, blind_validation())) as request:
                code, _, _ = self.invoke(args)
            self.assertEqual(code, 0)
            payload = request.call_args.args[1]
            self.assertEqual(set(payload), {"text", "candidate", "profile", "reader_profile", "validation_mode"})
            self.assertEqual(payload["validation_mode"], "blind")
            self.assertEqual(payload["reader_profile"], "explicit-reader")

    def test_blind_reader_profile_distinguishes_inheritance_and_explicit_clear(self):
        for option in ([], ["--reader-profile="]):
            with self.subTest(option=option), tempfile.TemporaryDirectory() as tmp:
                path = Path(tmp) / "blind.jsonl"
                args = self.live_args(path) + ["--validation-mode", "blind"] + option
                with patch.object(runner, "request_json", return_value=(200, blind_validation())) as request:
                    code, _, _ = self.invoke(args)
                self.assertEqual(code, 0)
                payload = request.call_args.args[1]
                manifest = json.loads(path.read_text().splitlines()[0])
                if option:
                    self.assertEqual(payload["reader_profile"], "")
                    self.assertEqual(manifest["reader_profile"], "")
                else:
                    self.assertNotIn("reader_profile", payload)
                    self.assertIsNone(manifest["reader_profile"])

    def test_explicit_empty_reader_profile_is_rejected_outside_live_blind_validation(self):
        with tempfile.TemporaryDirectory() as tmp:
            argument_sets = [
                ["--reader-profile="],
                self.live_args(Path(tmp) / "compare.jsonl") + ["--reader-profile="],
                self.live_args(Path(tmp) / "compile.jsonl", "compile") + [
                    "--validation-mode", "blind", "--reader-profile="],
            ]
            for args in argument_sets:
                with self.subTest(args=args), patch.object(runner, "request_json") as request:
                    code, _, _ = self.invoke(args)
                self.assertEqual(code, 1)
                request.assert_not_called()

    def test_compare_receipts_require_current_method_policy_and_consistent_issues(self):
        for status in ("passed", "failed"):
            body = validation(status)
            self.assertEqual(runner.validation_observation(200, body, "compare")[0], status)
            for field, value in (("method", "legacy_audit"), ("policy_version", "semantic-gate-v1"),
                                 ("issues", None)):
                with self.subTest(status=status, field=field):
                    changed = copy.deepcopy(body)
                    changed["validation"][field] = value
                    self.assertEqual(runner.validation_observation(200, changed, "compare")[0], "unknown")
        local = validation("failed")
        local["validation"]["method"] = "local_checks"
        self.assertEqual(runner.validation_observation(200, local, "compare")[0], "failed")
        local["validation"].update(status="passed", issues=[])
        self.assertEqual(runner.validation_observation(200, local, "compare")[0], "unknown")
        self.assertEqual(runner.validation_observation(200, blind_validation(), "compare")[0], "unknown")

    def test_blind_rejects_compare_and_legacy_receipts_even_when_top_level_passes(self):
        for status in ("passed", "failed"):
            with self.subTest(status=status):
                self.assertEqual(runner.validation_observation(200, validation(status), "blind")[0],
                                 "unknown")
        for field, value in (("method", "llm_compare"), ("method", None),
                             ("policy_version", "semantic-gate-v2"), ("policy_version", None)):
            with self.subTest(field=field, value=value):
                body = blind_validation()
                body["validation"][field] = value
                self.assertEqual(runner.validation_observation(200, body, "blind")[0], "unknown")

    def test_blind_success_requires_complete_current_stages_and_reader_receipt(self):
        body = blind_validation()
        category, detail = runner.validation_observation(200, body, "blind")
        self.assertEqual(category, "passed")
        self.assertEqual(detail["requested_mode"], "blind")
        self.assertEqual(detail["stages"][1]["reading"], body["validation"]["stages"][1]["reading"])
        changes = {
            "missing stages": lambda v: v.pop("stages"),
            "omitted stage": lambda v: v["stages"].pop(),
            "duplicate stage": lambda v: v["stages"][1].update(name="local_checks"),
            "non-string stage name": lambda v: v["stages"][1].update(name=[]),
            "old stage policy": lambda v: v["stages"][1].update(policy_version="blind-reader-v0"),
            "missing stage policy": lambda v: v["stages"][2].pop("policy_version"),
            "local checks skipped": lambda v: v["stages"][0].update(status="not_run"),
            "reader skipped": lambda v: v["stages"][1].update(status="not_run"),
            "reader approval is not completion": lambda v: v["stages"][1].update(status="passed"),
            "comparator not decided": lambda v: v["stages"][2].update(status="completed"),
            "missing reading": lambda v: v["stages"][1].pop("reading"),
            "invalid reading version": lambda v: v["stages"][1]["reading"].update(schema_version=True),
            "missing reading relations": lambda v: v["stages"][1]["reading"].pop("relations"),
            "empty reading relations": lambda v: v["stages"][1]["reading"].update(relations=[]),
            "unresolved reader ambiguity": lambda v: v["stages"][1]["reading"].update(ambiguities=["unclear"]),
            "success with issues": lambda v: v.update(issues=["conflicting issue"]),
            "stage success with issues": lambda v: v["stages"][2].update(issues=["conflicting issue"]),
        }
        for name, mutate in changes.items():
            with self.subTest(name=name):
                changed = blind_validation()
                mutate(changed["validation"])
                self.assertEqual(runner.validation_observation(200, changed, "blind")[0], "unknown")

    def test_blind_reading_allows_empty_entities_and_empty_relations_only_on_reader_rejection(self):
        body = blind_validation()
        body["validation"]["stages"][1]["reading"]["entities"] = []
        self.assertEqual(runner.validation_observation(200, body, "blind")[0], "passed")
        body = blind_rejection("reader")
        body["validation"]["stages"][1]["reading"]["relations"] = []
        self.assertEqual(runner.validation_observation(200, body, "blind")[0], "failed")
        body = blind_rejection("comparator")
        body["validation"]["stages"][1]["reading"]["relations"] = []
        self.assertEqual(runner.validation_observation(200, body, "blind")[0], "unknown")

    def test_blind_rejections_require_a_valid_early_stop_or_comparator_rejection(self):
        for stage_name in runner.BLIND_STAGE_POLICIES:
            with self.subTest(stage=stage_name):
                body = blind_rejection(stage_name)
                self.assertEqual(runner.validation_observation(200, body, "blind")[0], "failed")
                body["validation"]["status"] = "passed"
                body["validation"]["issues"] = []
                self.assertEqual(runner.validation_observation(200, body, "blind")[0], "unknown")
        body = blind_rejection("reader")
        body["validation"]["stages"][1]["reading"]["ambiguities"] = ["fixture ambiguity"]
        self.assertEqual(runner.validation_observation(200, body, "blind")[0], "failed")
        body["validation"]["stages"][2]["status"] = "passed"
        self.assertEqual(runner.validation_observation(200, body, "blind")[0], "unknown")
        body = blind_validation()
        body["validation"].update(status="failed", issues=["no failing stage"])
        self.assertEqual(runner.validation_observation(200, body, "blind")[0], "unknown")

    def test_blind_protocol_mismatch_and_service_error_are_excluded_from_agreement(self):
        failure = blind_rejection("reader")
        failure["validation"].update(status="error", issues=["provider secret fixture"])
        failure["validation"]["stages"][1].update(status="error", issues=["provider secret fixture"])
        responses = [(200, blind_validation()), (200, validation("failed")), (502, failure)]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "blind.jsonl"
            args = self.live_args(path) + ["--validation-mode", "blind", "--limit", "1"]
            args[args.index("--max-requests") + 1] = "3"
            with patch.object(runner, "request_json", side_effect=responses):
                code, output, _ = self.invoke(args)
            self.assertEqual(code, 2)
            summary = json.loads(output)
            self.assertEqual(summary["api_success"], 1)
            self.assertEqual(summary["draft_label_agreement_denominator"], 1)
            self.assertEqual(summary["draft_label_confusion_matrix"], {
                "draft_valid": {"passed": 1, "failed": 0},
                "draft_invalid": {"passed": 0, "failed": 0},
            })
            self.assertEqual(summary["excluded_from_agreement"], {"error": 1, "unknown": 1})
            records = [json.loads(line) for line in path.read_text().splitlines()]
            self.assertEqual(records[0]["expected_validation_policy"], runner.BLIND_POLICY)
            self.assertEqual(records[2]["validation"]["receipt_check"], "blind_method_or_policy_mismatch")
            self.assertEqual(records[3]["validation"]["receipt_check"], "transport_or_http_error")
            self.assertNotIn("provider secret fixture", path.read_text())

    def test_service_errors_never_count_as_semantic_rejections(self):
        for status in ("passed", "failed", "error"):
            with self.subTest(status=status):
                category, _ = runner.validation_observation(502, validation(status), "blind")
                self.assertEqual(category, "error")
        for top_status in ("passed", "failed"):
            body = blind_validation()
            body["validation"]["status"] = top_status
            body["validation"]["stages"][1].update(status="error", issues=["service failed"])
            self.assertEqual(runner.validation_observation(200, body, "blind")[0], "error")

    def test_http_502_json_validation_body_survives_request_error_path(self):
        body = validation("error", ["provider error fixture"])
        error = urllib.error.HTTPError("http://127.0.0.1:8080/api/validate", 502,
                                       "Bad Gateway", {}, io.BytesIO(json.dumps(body).encode()))
        with patch.object(runner.urllib.request.OpenerDirector, "open", side_effect=error):
            http_status, parsed = runner.request_json("http://127.0.0.1:8080/api/validate", {}, 1)
        self.assertEqual(http_status, 502)
        self.assertEqual(parsed, body)
        category, detail = runner.validation_observation(http_status, parsed, "blind")
        self.assertEqual(category, "error")
        self.assertTrue(detail["issues_omitted"])
        self.assertNotIn("provider error fixture", json.dumps(detail))

    def test_existing_output_is_never_overwritten(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "existing.jsonl"
            path.write_text("previous run\n")
            with patch.object(runner, "request_json") as request:
                code, _, _ = self.invoke(self.live_args(path))
            self.assertEqual(code, 1)
            self.assertEqual(path.read_text(), "previous run\n")
            request.assert_not_called()

    def test_endpoint_rejects_embedded_credentials_and_query(self):
        for value in ("http://name:password@example.test", "https://example.test?api_key=test",
                      "file:///tmp/api", "http://example.test/#fragment"):
            with self.subTest(endpoint=value), self.assertRaises(ValueError):
                runner.validate_endpoint(value)

    def test_unrecognized_validation_is_unknown_not_a_correct_rejection(self):
        for body in ({}, {"validation": {"status": "FAIL"}}, {"validation": {"status": False}},
                     {"validation": "PASS"}):
            result, _ = runner.validation_observation(200, body)
            self.assertEqual(result, "unknown")


if __name__ == "__main__":
    unittest.main()

