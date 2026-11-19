import contextlib
import copy
import importlib.util
import io
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from html.parser import HTMLParser
from pathlib import Path
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("babel_eval_report", Path(__file__).with_name("report.py"))
report = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(report)


class Node:
    def __init__(self, tag, attrs=()):
        self.tag, self.attrs, self.children = tag, dict(attrs), []

    def text(self):
        return "".join(child.text() if isinstance(child, Node) else child for child in self.children)

    def find(self, tag=None, **attrs):
        found = []
        if (tag is None or self.tag == tag) and all(self.attrs.get(key) == value for key, value in attrs.items()):
            found.append(self)
        for child in self.children:
            if isinstance(child, Node):
                found.extend(child.find(tag, **attrs))
        return found


class Document(HTMLParser):
    VOID = {"meta", "link", "input", "br", "hr", "img", "source", "wbr", "area", "base", "embed", "param", "track", "col"}

    def __init__(self, markup):
        super().__init__(convert_charrefs=True)
        self.root = Node("document")
        self.stack = [self.root]
        self.feed(markup)
        self.close()

    def handle_starttag(self, tag, attrs):
        node = Node(tag, attrs)
        self.stack[-1].children.append(node)
        if tag not in self.VOID:
            self.stack.append(node)

    def handle_endtag(self, tag):
        for index in range(len(self.stack) - 1, 0, -1):
            if self.stack[index].tag == tag:
                self.stack = self.stack[:index]
                break

    def handle_data(self, data):
        self.stack[-1].children.append(data)


def stats(encoding="cl100k_base", kind="exact", source="tiktoken-go/tokenizer", original=30, candidate=20):
    return {"original_tokens": original, "compiled_tokens": candidate,
            "measurement_kind": kind, "measurement_encoding": encoding,
            "measurement_source": source, "measurement_scope": "text",
            "measurement_version": "v0.3.0" if kind == "exact" else "2"}


def receipt(status="passed", blind=False):
    check = "recognized_compare_receipt" if not blind else "recognized_blind_receipt" if status == "passed" else "recognized_blind_comparator_rejection"
    issues = ["fixture rejection"] if status == "failed" else []
    value = {"requested_mode": "blind" if blind else "compare", "receipt_check": check,
             "status": status, "method": "blind_read" if blind else "llm_compare",
             "policy_version": "semantic-blind-v1" if blind else "semantic-gate-v2", "issues": issues}
    if blind:
        value["stages"] = [
            {"name": "local_checks", "status": "completed", "policy_version": "semantic-gate-v2", "issues": [], "duration_ms": 1},
            {"name": "reader", "status": "completed", "policy_version": "blind-reader-v1", "issues": [],
             "model": "reader-fixture", "duration_ms": 12,
             "reading": {"schema_version": 1, "entities": ["account"], "ambiguities": [],
                         "relations": [{"subject": "account", "relation": "requires", "object": "approval", "evidence": "needs approval"}]}},
            {"name": "comparator", "status": status, "policy_version": "blind-comparator-v1", "issues": issues,
             "model": "comparator-fixture", "duration_ms": 19},
        ]
    return value


def observation(case_id="case-one", result="api_success", *, pipeline=False, counts=None):
    value = {"record_type": "observation", "case_id": case_id, "candidate_id": "generated",
             "source": "  source  text\nnext line\n", "candidate": " actual  API candidate\n",
             "result": result, "http_status": 200, "elapsed_seconds": 0.25,
             "stats": counts or stats(), "validation": receipt(result),
             "expected_relations": [{"id": "r1", "statement": "Keep the condition."}],
             "protected_literals": ["  "]}
    if pipeline:
        value.update(operation="pipeline", validation=receipt(result, blind=True),
                     stages={"compile": {"result": "api_success", "http_status": 200, "elapsed_seconds": 0.1},
                             "validate": {"result": result, "http_status": 200, "elapsed_seconds": 0.15}},
                     measurement={"status": "included", "reason": None,
                                  "group": {key: value["stats"][key] for key in report.META_FIELDS},
                                  "original_tokens": value["stats"]["original_tokens"],
                                  "candidate_tokens": value["stats"]["compiled_tokens"],
                                  "effective_tokens": value["stats"]["compiled_tokens"] if result == "passed" else value["stats"]["original_tokens"]},
                     selection={"output": "candidate" if result == "passed" else "source", "reason": "fixture_rule"})
    return value


def records(operation, rows, *, final=True, summary_updates=None):
    manifest = {"record_type": "manifest", "format_version": 2 if operation == "pipeline" else 1,
                "operation": operation, "annotation_status": "assistant_drafted_requires_human_review",
                "profile": "compiler-fixture", "validation_profile": "comparator-fixture",
                "validation_mode": "blind" if operation == "pipeline" else "compare",
                "reader_profile": None, "dataset_sha256": "a" * 64,
                "endpoint": "https://internal-private-endpoint.example/v1",
                "started_at_utc": "2026-01-01T00:00:00+00:00", "planned_requests": len(rows),
                "selection_scope": "evaluation_rule_only"}
    result = [manifest, *copy.deepcopy(rows)]
    if final:
        summary = {"record_type": "summary", "operation": operation, "requests": len(rows),
                   "cases": len(rows), "planned_requests": len(rows), "skipped_by_budget": 0,
                   "error": sum(row.get("result") == "error" for row in rows), "unknown": 0}
        summary.update(summary_updates or {})
        result.append(summary)
    return result


class ReportTests(unittest.TestCase):
    def render(self, items, name="run.jsonl"):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / name
            path.write_text("".join(json.dumps(item, ensure_ascii=False) + "\n" for item in items), encoding="utf-8")
            run = report.load_run(path)
            markup = report.build_html(run, name)
        return run, markup, Document(markup).root

    def invoke(self, args):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = report.main(args)
        return code, out.getvalue(), err.getvalue()

    def test_all_untrusted_text_is_dom_text_not_tags_attributes_or_scripts(self):
        attack = '\n  </code></pre></script><script>alert("x")</script><img src=x onerror="boom"> & \' "\u2028\u2029  \n'
        row = observation(attack, "passed", pipeline=True)
        row.update(source=attack, candidate=attack, expected_relations=[{"id": attack, "statement": attack}],
                   protected_literals=[attack])
        row["validation"]["issues"] = [attack]
        row["validation"]["stages"][1]["model"] = attack
        row["validation"]["stages"][1]["reading"]["entities"] = [attack]
        row["measurement"]["group"]["measurement_source"] = attack
        row["measurement"]["group"]["measurement_version"] = attack
        items = records("pipeline", [row])
        items[0]["profile"] = attack
        items[0]["annotation_status"] = attack
        _, markup, root = self.render(items, name='run-"<&.jsonl')
        self.assertEqual(len(root.find("script")), 1)
        self.assertEqual(root.find("script")[0].text(), report.SCRIPT)
        self.assertEqual(len(root.find("style")), 1)
        self.assertEqual(root.find("style")[0].text(), report.CSS)
        self.assertFalse(root.find("img"))
        self.assertFalse(root.find("iframe"))
        for node in root.find():
            self.assertFalse(any(key.lower().startswith("on") for key in node.attrs))
            self.assertFalse(any(key in node.attrs for key in ("src", "href", "srcdoc")))
        self.assertEqual(root.find("code", **{"data-text-role": "source"})[0].text(), attack)
        self.assertEqual(root.find("code", **{"data-text-role": "candidate"})[0].text(), attack)
        csp = root.find("meta", **{"http-equiv": "Content-Security-Policy"})[0].attrs["content"]
        self.assertIn("connect-src 'none'", csp)
        self.assertIn("script-src 'sha256-", csp)
        self.assertNotIn("internal-private-endpoint", markup)

    def test_actual_candidate_and_leading_whitespace_are_preserved_for_both_v1_modes(self):
        for operation, result in (("compile", "api_success"), ("validate", "failed")):
            with self.subTest(operation=operation):
                row = observation(result=result)
                row["source"] = "\n\n  before  \t after\n"
                row["candidate"] = "\n actual   returned candidate\n  "
                row["valid_candidate"] = "NEVER DISPLAY THIS FIXTURE SUBSTITUTE"
                _, markup, root = self.render(records(operation, [row]))
                self.assertEqual(root.find("code", **{"data-text-role": "source"})[0].text(), row["source"])
                self.assertEqual(root.find("code", **{"data-text-role": "candidate"})[0].text(), row["candidate"])
                self.assertNotIn("NEVER DISPLAY", markup)

    def test_empty_results_and_interrupted_run_do_not_invent_final_metrics(self):
        run, _, root = self.render(records("validate", []))
        self.assertFalse(run["warnings"])
        self.assertEqual(len(root.find("section", id="empty-results")), 1)
        self.assertEqual(len(root.find("article")), 0)
        run, _, root = self.render(records("pipeline", [observation(result="error", pipeline=True)], final=False))
        self.assertIsNone(run["summary"])
        self.assertTrue(any("缺少最终 summary" in message for message in run["warnings"]))
        self.assertEqual(len(root.find("article")), 1)
        self.assertNotIn("100.0%", root.text())

    def test_pipeline_stage_errors_and_reading_remain_distinct(self):
        row = observation(result="error", pipeline=True)
        row["stages"]["validate"].update(result="error", http_status=502)
        row["validation"].update(status="error", issues_omitted=True)
        row["validation"]["stages"][1].update(status="error", issues_omitted=True)
        row["validation"]["stages"][1].pop("reading")
        row["validation"]["stages"][2].update(status="not_run", duration_ms=None)
        _, _, root = self.render(records("pipeline", [row]))
        card = root.find("article")[0]
        self.assertEqual(card.attrs["data-result"], "error")
        stages = card.find("section", **{"class": "stage"})
        self.assertEqual(len(stages), 5)
        reader = next(node for node in stages if node.find("h4")[0].text() == "独立阅读器")
        comparator = next(node for node in stages if node.find("h4")[0].text() == "对照器")
        self.assertIn("异常", reader.text())
        self.assertIn("12 ms", reader.text())
        self.assertIn("未执行", comparator.text())
        self.assertIn("运行器已省略阶段异常详情", reader.text())
        self.assertFalse(any("reading" in node.find("summary")[0].text() for node in card.find("details")))
        _, _, good = self.render(records("pipeline", [observation(result="passed", pipeline=True)]))
        reading = next(node for node in good.find("details") if "reading" in node.find("summary")[0].text())
        self.assertNotIn("open", reading.attrs)
        data = json.loads(reading.find("code")[0].text())
        self.assertEqual(data["entities"], ["account"])
        self.assertIn("按评测规则选择（未发送下游）", good.text())

    def test_mixed_counts_keep_three_separate_bases_and_excluded_metadata_is_not_exact(self):
        variants = [stats("estimate", "estimated", "heuristic-v2", 300, 100),
                    stats("cl100k_base", original=40, candidate=20),
                    stats("o200k_base", original=20, candidate=10)]
        rows = [observation(f"case-{i}", "passed", pipeline=True, counts=value) for i, value in enumerate(variants)]
        groups = [{**row["measurement"]["group"], "cases": 1,
                   "original_tokens": row["measurement"]["original_tokens"],
                   "candidate_tokens": row["measurement"]["candidate_tokens"],
                   "effective_tokens": row["measurement"]["candidate_tokens"],
                   "adopted_candidates": 1, "fallback_to_source": 0} for row in rows]
        excluded = observation("contradictory", "passed", pipeline=True,
                               counts=stats("estimate", "exact", "heuristic-v2"))
        excluded["measurement"] = {"status": "excluded", "reason": "inconsistent_measurement_metadata"}
        excluded["selection"] = {"output": "source", "reason": "measurement_unavailable"}
        rows.append(excluded)
        _, _, root = self.render(records("pipeline", rows, summary_updates={"measurement_groups": groups}))
        rendered_groups = root.find("section", **{"class": "measurement-group"})
        self.assertEqual(len(rendered_groups), 3)
        for node, variant in zip(rendered_groups, variants):
            self.assertIn(variant["measurement_encoding"], [dd.text() for dd in node.find("dd")])
        self.assertEqual(len(root.find("p", **{"class": "token-count"})), 3)
        self.assertIn("原文 300 → 候选 100 tok", root.text())  # Never recounted from text length.
        excluded_card = root.find("article")[-1]
        self.assertFalse(excluded_card.find("p", **{"class": "token-count"}))
        self.assertIn("inconsistent_measurement_metadata", excluded_card.text())
        self.assertTrue(report.measurement_label(excluded["stats"]).startswith("计量声明矛盾"))
        self.assertNotIn("实际账单节省", root.text())

    def test_summary_rates_keep_the_recorded_denominator_and_no_semantic_score(self):
        rows = [observation("a", "passed", pipeline=True), observation("b", "failed", pipeline=True),
                observation("c", "error", pipeline=True)]
        _, _, root = self.render(records("pipeline", rows, summary_updates={
            "validation_pass_rate": 0.5, "validation_decided": 2,
            "excluded_from_validation": {"error": 1, "unknown": 0, "not_validated": 0}}))
        headings = [node.text() for node in root.find("dt")]
        self.assertIn("所选验证流程通过率", headings)
        self.assertNotIn("语义准确率", headings)
        self.assertIn("50.0%（记录分母 2）", [node.text() for node in root.find("dd")])

    def test_contradictory_positive_receipt_becomes_unknown_and_cannot_inherit_summary_pass_rate(self):
        row = observation(result="passed", pipeline=True)
        row["validation"] = {"status": "passed", "method": "llm_compare", "policy_version": "semantic-gate-v2",
                             "requested_mode": "blind", "receipt_check": "blind_method_or_policy_mismatch", "issues": []}
        run, _, root = self.render(records("pipeline", [row], summary_updates={
            "validation_pass_rate": 1, "validation_decided": 1,
            "excluded_from_validation": {"error": 0, "unknown": 0, "not_validated": 0}}))
        checked = run["checked_summary"]
        self.assertEqual(run["observations"][0]["result"], "passed")  # Original evidence is retained.
        self.assertEqual(run["observations"][0]["_display_result"], "unknown")
        self.assertEqual(checked["validation_decided"], 0)
        self.assertIsNone(checked["validation_pass_rate"])
        self.assertEqual(checked["excluded_from_validation"]["unknown"], 1)
        self.assertEqual(checked["selection_counts"], {"candidate": 0, "source": 1})
        self.assertEqual(checked["measurement_groups"][0]["effective_tokens"], row["stats"]["original_tokens"])
        self.assertEqual(root.find("article")[0].attrs["data-result"], "unknown")
        self.assertNotIn("100.0%", root.text())
        self.assertIn("blind_method_or_policy_mismatch", root.text())
        self.assertTrue(any("原始汇总与复核后的观察不一致" in item for item in run["warnings"]))

    def test_current_pipeline_runner_output_is_consumed_without_an_adapter_or_network(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "pipeline.jsonl"
            actual = "\n  generated  candidate\n"
            responses = [(200, {"compiled": actual, "stats": stats()}),
                         (200, {"validation": {"status": "passed", "method": "llm_compare",
                                               "policy_version": "semantic-gate-v2", "issues": []}})]
            args = ["--live", "pipeline", "--endpoint", "http://127.0.0.1:1", "--profile", "fixture",
                    "--limit", "1", "--max-requests", "2", "--output", str(path)]
            with patch.object(report.RUNNER, "request_json", side_effect=responses) as request, \
                    contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                code = report.RUNNER.main(args)
            self.assertEqual(code, 0)
            self.assertEqual(request.call_count, 2)
            run = report.load_run(path)
            self.assertFalse(run["warnings"])
            self.assertEqual(run["checked_summary"]["validation_decided"], 1)
            self.assertEqual(run["observations"][0]["_display_selection"]["output"], "candidate")
            root = Document(report.build_html(run, path.name)).root
            self.assertEqual(root.find("code", **{"data-text-role": "candidate"})[0].text(), actual)

    def test_malformed_or_unknown_files_fail_before_output_is_created(self):
        samples = ["", "{}\n", "[]\n", '{"record_type":"manifest","record_type":"summary"}\n',
                   '{"record_type":"manifest","operation":"compile","format_version":99}\n',
                   '{"record_type":"manifest","operation":"compile","format_version":1,"x":NaN}\n']
        valid = records("compile", [observation()])
        samples.extend([json.dumps(valid[0]) + '\n{"record_type":',
                        "\n".join(json.dumps(row) for row in [*valid, valid[1]])])
        with tempfile.TemporaryDirectory() as directory:
            path, output = Path(directory) / "bad.jsonl", Path(directory) / "bad.html"
            for content in samples:
                with self.subTest(content=content):
                    path.write_text(content, encoding="utf-8")
                    code, _, error = self.invoke([str(path), "--output", str(output)])
                    self.assertEqual(code, 1)
                    self.assertIn("report error:", error)
                    self.assertFalse(output.exists())

    def test_unknown_observation_result_and_missing_text_are_visible_not_invented(self):
        row = observation()
        row.update(result={"unexpected": "value"}, source=None, candidate=None)
        run, _, root = self.render(records("compile", [row]))
        self.assertTrue(run["warnings"])
        self.assertEqual(root.find("article")[0].attrs["data-result"], "unknown")
        self.assertFalse(root.find("code", **{"data-text-role": "source"}))
        self.assertFalse(root.find("code", **{"data-text-role": "candidate"}))

    def test_cli_creates_new_report_offline_and_refuses_existing_output_or_input_path(self):
        with tempfile.TemporaryDirectory() as directory:
            path, output = Path(directory) / "run.jsonl", Path(directory) / "nested" / "report.html"
            source = "".join(json.dumps(item) + "\n" for item in records("compile", [observation()]))
            path.write_text(source, encoding="utf-8")
            with patch("socket.create_connection", side_effect=AssertionError("network use")), \
                    patch("urllib.request.urlopen", side_effect=AssertionError("network use")):
                code, _, error = self.invoke([str(path), "--output", str(output)])
            self.assertEqual(code, 0, error)
            saved = output.read_bytes()
            self.assertEqual(len(Document(saved.decode()).root.find("article")), 1)
            code, _, error = self.invoke([str(path), "--output", str(output)])
            self.assertEqual(code, 1)
            self.assertEqual(output.read_bytes(), saved)
            code, _, _ = self.invoke([str(path), "--output", str(path)])
            self.assertEqual(code, 1)
            self.assertEqual(path.read_text(encoding="utf-8"), source)
            process = subprocess.run([sys.executable, str(Path(report.__file__)), str(path),
                                      "--output", str(Path(directory) / "cli.html")],
                                     capture_output=True, text=True, check=False)
            self.assertEqual(process.returncode, 0, process.stderr)
            self.assertEqual(len(Document((Path(directory) / "cli.html").read_text()).root.find("article")), 1)

    @unittest.skipUnless(shutil.which("node"), "optional Node runtime unavailable for shipped JavaScript behavior test")
    def test_shipped_search_and_filter_script_changes_visible_cards(self):
        rows = [observation("alpha", "passed"), observation("beta", "error"), observation("gamma", "unknown")]
        rows[0]["source"] = "needle FOR Alpha"
        rows[1]["source"] = "needle FOR Beta"
        _, _, root = self.render(records("validate", rows))
        cards = [{"result": node.attrs["data-result"], "text": node.text()} for node in root.find("article")]
        payload = {"script": root.find("script")[0].text(), "cards": cards,
                   "queries": [{"result": "all", "search": "NEEDLE"},
                               {"result": "error", "search": "needle"},
                               {"result": "passed", "search": "Beta"},
                               {"result": "all", "search": ""}]}
        js = """
const fs = require('fs'), vm = require('vm');
const data = JSON.parse(fs.readFileSync(0, 'utf8'));
const cards = data.cards.map(card => ({dataset:{result:card.result}, textContent:card.text, hidden:false}));
const controls = {};
for (const id of ['search','result-filter','visible-count','no-matches'])
  controls[id] = {value:id==='result-filter'?'all':'', handlers:{}, addEventListener(name, fn){this.handlers[name]=fn;}};
vm.runInNewContext(data.script, {document:{querySelectorAll:()=>cards, getElementById:id=>controls[id]}});
const states = [];
for (const query of data.queries) {
  controls.search.value=query.search; controls['result-filter'].value=query.result;
  controls.search.handlers.input(); controls['result-filter'].handlers.change();
  states.push({visible:cards.map((card,i)=>card.hidden?null:i).filter(i=>i!==null),
    message:controls['visible-count'].textContent, noMatchesHidden:controls['no-matches'].hidden});
}
process.stdout.write(JSON.stringify(states));
"""
        process = subprocess.run([shutil.which("node"), "-e", js], input=json.dumps(payload),
                                 capture_output=True, text=True, check=False)
        self.assertEqual(process.returncode, 0, process.stderr)
        states = json.loads(process.stdout)
        self.assertEqual([state["visible"] for state in states], [[0, 1], [1], [], [0, 1, 2]])
        self.assertEqual(states[2]["message"], "显示 0 / 3 条观察")
        self.assertFalse(states[2]["noMatchesHidden"])


if __name__ == "__main__":
    unittest.main()
