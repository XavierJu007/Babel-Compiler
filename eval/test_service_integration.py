"""Opt-in real Babel HTTP integration; every upstream model is a local fixture."""
import collections
import http.server
import json
import os
from pathlib import Path
import queue
import re
import subprocess
import sys
import tempfile
import threading
import unittest


BINARY = os.environ.get("BABEL_TEST_BINARY")
EVAL_DIR = Path(__file__).resolve().parent
CANDIDATE = "certificate expired → service unavailable.\nRestart only after certificate replacement.\n"


@unittest.skipUnless(BINARY, "set BABEL_TEST_BINARY to a built Babel binary for HTTP integration")
class RealServicePipelineTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="babel-pipeline-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.calls = collections.Counter()
        self.receipts = []
        self.provider_errors = []
        test = self

        class Provider(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                try:
                    body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                    model = body["model"]
                    test.calls[model] += 1
                    test.assertEqual(self.headers.get("Authorization"), "Bearer mock-" + model)
                    if model == "compiler-fixture":
                        answer = CANDIDATE
                    elif model == "reader-fixture":
                        data = json.loads(body["messages"][1]["content"])
                        test.assertEqual(data, {"candidate": CANDIDATE})
                        test.receipts.append(data)
                        answer = json.dumps({
                            "schema_version": 1, "entities": ["certificate", "service"],
                            "relations": [
                                {"subject": "certificate", "relation": "expired", "object": "",
                                 "conditions": [], "scope": "cause of service unavailability",
                                 "modality": "fact", "negated": False, "quantities": [],
                                 "evidence": "certificate expired → service unavailable."},
                                {"subject": "", "relation": "Restart", "object": "",
                                 "conditions": ["only after certificate replacement"], "scope": "",
                                 "modality": "obligation", "negated": False, "quantities": [],
                                 "evidence": "Restart only after certificate replacement."},
                            ], "ambiguities": [],
                        })
                    elif model == "checker-fixture":
                        data = json.loads(body["messages"][1]["content"])
                        test.assertEqual(set(data), {"original", "reading"})
                        test.assertIn("certificate expired", data["original"])
                        test.receipts.append(data)
                        answer = '{"pass":true,"missing":[]}'
                    else:
                        raise AssertionError("unexpected configured model: " + model)
                    response = json.dumps({"choices": [{"finish_reason": "stop", "message": {"content": answer}}]}).encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(response)
                except Exception as exc:
                    test.provider_errors.append(str(exc))
                    self.send_error(500, "fixture rejected the request")

        provider = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
        provider_thread = threading.Thread(target=lambda: provider.serve_forever(poll_interval=0.05), daemon=True)
        provider_thread.start()

        def close_provider():
            provider.shutdown()
            provider.server_close()
            provider_thread.join(timeout=2)

        self.addCleanup(close_provider)
        base = f"http://127.0.0.1:{provider.server_port}"
        profiles = {name: {"base_url": base, "api_key": "mock-" + model, "model": model}
                    for name, model in (("compiler", "compiler-fixture"), ("reader", "reader-fixture"),
                                        ("checker", "checker-fixture"))}
        (self.root / "babel.config.json").write_text(json.dumps({"active_profile": "compiler", "profiles": profiles}))
        environment = dict(os.environ, HOME=str(self.root), USERPROFILE=str(self.root))
        self.service = subprocess.Popen([str(Path(BINARY).resolve()), "-serve", "127.0.0.1:0"],
                                        cwd=self.root, env=environment, stdout=subprocess.PIPE,
                                        stderr=subprocess.STDOUT, text=True, encoding="utf-8")

        def stop_service():
            if self.service.poll() is None:
                self.service.terminate()
                try:
                    self.service.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    self.service.kill()
                    self.service.wait(timeout=5)
            self.service.stdout.close()

        self.addCleanup(stop_service)
        lines = queue.Queue()

        def read_startup():
            for line in self.service.stdout:
                lines.put(line)
            lines.put(None)

        threading.Thread(target=read_startup, daemon=True).start()
        for _ in range(20):
            line = lines.get(timeout=5)
            self.assertIsNotNone(line, "Babel exited before starting its HTTP listener")
            found = re.search(r"http://127\.0\.0\.1:([1-9][0-9]*)", line)
            if found:
                self.endpoint = found.group(0)
                break
        else:
            self.fail("Babel did not report its actual listening address")

    def run_pipeline(self, budget):
        output = self.root / "run.jsonl"
        command = [sys.executable, str(EVAL_DIR / "run.py"), "--live", "pipeline",
                   "--endpoint", self.endpoint, "--profile", "compiler", "--validation-profile", "checker",
                   "--validation-mode", "blind", "--reader-profile", "reader", "--token-encoding", "o200k_base",
                   "--case-id", "en-cause-and-recovery", "--limit", "1", "--max-requests", str(budget),
                   "--output", str(output)]
        process = subprocess.run(command, capture_output=True, text=True, encoding="utf-8", timeout=15)
        self.assertEqual(process.returncode, 0, process.stderr)
        self.assertEqual(self.provider_errors, [])
        return output, [json.loads(line) for line in output.read_text(encoding="utf-8").splitlines()]

    def test_real_service_pipeline_to_html_report(self):
        output, records = self.run_pipeline(2)
        manifest, record, summary = records
        self.assertEqual(record["candidate"], CANDIDATE)
        self.assertEqual(record["result"], "passed")
        self.assertEqual(record["validation"]["method"], "blind_read")
        self.assertEqual(record["stats"]["measurement_encoding"], "o200k_base")
        self.assertEqual(record["measurement"]["status"], "included")
        self.assertEqual(record["selection"]["output"], "candidate")
        self.assertEqual(manifest["selection_scope"], "evaluation_rule_only")
        self.assertEqual(summary["requests"], 2)
        self.assertEqual(summary["semantic_accuracy"], "not_established")
        self.assertEqual(self.calls, {"compiler-fixture": 1, "reader-fixture": 1, "checker-fixture": 1})
        html = self.root / "report.html"
        report = subprocess.run([sys.executable, str(EVAL_DIR / "report.py"), str(output), "--output", str(html)],
                                capture_output=True, text=True, encoding="utf-8", timeout=10)
        self.assertEqual(report.returncode, 0, report.stderr)
        rendered = html.read_text(encoding="utf-8")
        self.assertIn("en-cause-and-recovery", rendered)
        self.assertIn("o200k_base", rendered)
        self.assertIn("certificate expired", rendered)

    def test_real_service_budget_keeps_generated_candidate_unvalidated(self):
        _, records = self.run_pipeline(1)
        record, summary = records[1:]
        self.assertEqual(record["candidate"], CANDIDATE)
        self.assertEqual(record["result"], "not_validated")
        self.assertEqual(record["selection"]["output"], "source")
        self.assertEqual(summary["requests"], 1)
        self.assertEqual(summary["validate_requests"], 0)
        self.assertIsNone(summary["validation_pass_rate"])
        self.assertEqual(self.calls, {"compiler-fixture": 1})


if __name__ == "__main__":
    unittest.main()
