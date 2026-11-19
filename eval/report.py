#!/usr/bin/env python3
"""Render an existing Babel compile/validate/pipeline JSONL run as offline HTML.

Usage: python eval/report.py RUN.jsonl --output report.html
Uses only the Python standard library. Never calls a model, reads configuration,
fetches resources, recomputes token counts, or overwrites an existing output.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import html
import importlib.util
import json
import math
import sys
from collections import Counter
from pathlib import Path


# Reuse the runner's pure receipt/measurement rules. Importing the sibling module
# does not invoke main(), read model configuration, or make any requests.
_RUNNER_SPEC = importlib.util.spec_from_file_location("babel_eval_report_rules", Path(__file__).with_name("run.py"))
RUNNER = importlib.util.module_from_spec(_RUNNER_SPEC)
_RUNNER_SPEC.loader.exec_module(RUNNER)


RESULTS = {
    "api_success": "生成 API 成功 · 语义未评估",
    "passed": "所选验证流程通过",
    "failed": "所选验证流程未通过",
    "error": "执行异常",
    "unknown": "结果未知",
    "not_validated": "未执行验证",
}
META_FIELDS = ("measurement_kind", "measurement_source", "measurement_scope",
               "measurement_encoding", "measurement_version")
STAGE_NAMES = {"compile": "生成 API", "validate": "验证 API",
               "local_checks": "本地检查", "reader": "独立阅读器", "comparator": "对照器"}
STAGE_STATUSES = {"not_run": "未执行", "completed": "已完成", "passed": "通过",
                  "failed": "未通过", "error": "异常", "unknown": "未知",
                  "api_success": "API 成功", "not_validated": "未验证"}

CSS = """
:root{font-family:system-ui,-apple-system,'Segoe UI',sans-serif;color:#172033;background:#f4f6f8;line-height:1.55}
body{max-width:1180px;margin:0 auto;padding:24px}h1{margin:0 0 8px}h2{font-size:1.2rem}h3{font-size:1rem}
header,section.overview,article.case{background:white;border:1px solid #dce2e9;border-radius:8px;padding:20px;margin:16px 0}
.muted{color:#596579}.note,.warning{padding:12px;border-left:4px solid #b98527;background:#fff8e9;overflow-wrap:anywhere}
.warning{border-color:#b24545;background:#fff1f1}dl{display:grid;grid-template-columns:minmax(130px,auto) 1fr;gap:5px 18px;margin:12px 0}
dt{color:#596579}dd{margin:0;overflow-wrap:anywhere}.filters{display:flex;flex-wrap:wrap;gap:12px;align-items:center}
input,select{font:inherit;border:1px solid #b9c3d0;border-radius:4px;padding:7px;max-width:100%}input{min-width:200px}
.case-head{display:flex;flex-wrap:wrap;justify-content:space-between;gap:10px}.badge{border-radius:4px;background:#eef1f6;padding:3px 8px;font-size:.9rem}
.result-passed{background:#e7f0fb}.result-failed,.result-error{background:#fbe8e8}.columns{display:grid;grid-template-columns:1fr 1fr;gap:16px}
.columns>section{min-width:0}pre{background:#f7f8fa;border:1px solid #e1e5eb;padding:12px;margin:8px 0;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;tab-size:4}
code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace}table{border-collapse:collapse;width:100%;margin:10px 0}th,td{border:1px solid #dce2e9;padding:7px;text-align:left;vertical-align:top;overflow-wrap:anywhere}
details{margin:10px 0}summary{cursor:pointer;color:#255e92}.measurement-group{border-top:1px solid #dce2e9;padding-top:12px;margin-top:14px}
.stage{border-left:2px solid #dce2e9;padding-left:12px;margin:12px 0}[hidden]{display:none!important}footer{color:#596579;margin:24px 0;overflow-wrap:anywhere}
@media(max-width:700px){body{padding:12px}.columns{grid-template-columns:1fr}dl{grid-template-columns:1fr;gap:2px}dd{margin-bottom:6px}}
""".strip()

# No run data is embedded in JavaScript, CSS, URLs, event handlers, or HTML
# identifiers. Search uses the already escaped DOM text; it never uses innerHTML.
SCRIPT = """
(() => {
  const cards = Array.from(document.querySelectorAll('[data-case]'));
  const search = document.getElementById('search');
  const result = document.getElementById('result-filter');
  const count = document.getElementById('visible-count');
  const noMatches = document.getElementById('no-matches');
  function applyFilters() {
    const query = search.value.trim().toLocaleLowerCase();
    let visible = 0;
    cards.forEach(card => {
      const matches = (result.value === 'all' || card.dataset.result === result.value) &&
        (!query || card.textContent.toLocaleLowerCase().includes(query));
      card.hidden = !matches;
      if (matches) visible++;
    });
    count.textContent = `显示 ${visible} / ${cards.length} 条观察`;
    noMatches.hidden = visible !== 0 || cards.length === 0;
  }
  search.addEventListener('input', applyFilters);
  result.addEventListener('change', applyFilters);
  applyFilters();
})();
""".strip()


class ReportError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise ReportError(message)


def plain(value):
    if value is None:
        return "未提供"
    if isinstance(value, str):
        return value
    return json.dumps(value, ensure_ascii=False, allow_nan=False, sort_keys=True)


def esc(value):
    return html.escape(plain(value), quote=True)


def nonnegative_int(value):
    return type(value) is int and value >= 0


def result_category(value):
    return value if isinstance(value, str) and value in RESULTS else "unknown"


def duration(value, unit="seconds"):
    if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
        return "未记录"
    return f"{value:g} ms" if unit == "ms" else f"{value:.3f} s"


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        require(key not in value, f"duplicate JSON key: {key}")
        value[key] = item
    return value


def reject_constant(value):
    raise ReportError(f"non-finite JSON number: {value}")


def check_observation(row, manifest):
    """Check protocol consistency, without interpreting meaning or retokenizing."""
    operation, recorded = manifest["operation"], row.get("result")
    category = result_category(recorded)
    allowed = {"compile": ("api_success", "error", "unknown"),
               "validate": ("passed", "failed", "error", "unknown"),
               "pipeline": ("passed", "failed", "error", "unknown", "not_validated")}
    reason = None
    if recorded not in allowed[operation]:
        category, reason = "unknown", "unexpected_result_for_operation"
    elif recorded in ("api_success", "passed", "failed") and not (
            RUNNER.is_text(row.get("source")) and RUNNER.is_text(row.get("candidate"))):
        category, reason = "unknown", "missing_source_or_actual_candidate"
    elif recorded == "api_success" and row.get("http_status") != 200:
        category, reason = "unknown", "generation_http_status_mismatch"
    elif recorded in ("passed", "failed"):
        validation = row.get("validation")
        mode = manifest.get("validation_mode")
        http_status = row.get("http_status")
        if operation == "pipeline":
            stages = row.get("stages")
            stage = stages.get("validate") if isinstance(stages, dict) else None
            http_status = stage.get("http_status") if isinstance(stage, dict) else None
        if mode not in ("compare", "blind") or not isinstance(validation, dict):
            category, reason = "unknown", "missing_requested_mode_or_validation_receipt"
        else:
            recognized, detail = RUNNER.validation_observation(http_status, {"validation": validation}, mode)
            if recognized != recorded:
                category, reason = "unknown", detail["receipt_check"]
            elif validation.get("requested_mode") != mode:
                category, reason = "unknown", "recorded_requested_mode_mismatch"
            elif validation.get("receipt_check") != detail["receipt_check"]:
                category, reason = "unknown", "recorded_receipt_check_mismatch"
    row["_display_result"] = category
    if reason:
        row["_warnings"].append(f"原始 result={plain(recorded)} 无法认可：{reason}。展示为 unknown，并排除验证通过率分母。")
    if operation == "pipeline":
        measurement, selection = RUNNER.pipeline_measurement(
            row.get("stats"), category, candidate_available=RUNNER.is_text(row.get("candidate")))
        row["_display_measurement"], row["_display_selection"] = measurement, selection
        for key, checked in (("measurement", measurement), ("selection", selection)):
            original = row.get(key)
            if not isinstance(original, dict) or any(original.get(field) != value for field, value in checked.items()):
                row["_warnings"].append(f"原始 {key} 与同一评测规则的复核结果不一致；正文展示复核结果，原记录保留在明细中。")


def checked_summary(summary, observations, operation):
    """Never let a supplied summary override the recognized observations."""
    if summary is None:
        return None
    counts = Counter(row["_display_result"] for row in observations)
    checked = {key: summary.get(key) for key in ("requests", "planned_requests", "skipped_by_budget")}
    checked.update(error=counts["error"], unknown=counts["unknown"], not_validated=counts["not_validated"])
    if operation == "pipeline":
        decided = counts["passed"] + counts["failed"]
        groups, excluded = {}, {}
        selection_counts = {"candidate": 0, "source": 0}
        for row in observations:
            RUNNER.add_pipeline_measurement(groups, excluded, row["_display_measurement"], row["_display_selection"])
            selection_counts[row["_display_selection"]["output"]] += 1
        checked.update(validation_decided=decided,
                       validation_pass_rate=counts["passed"] / decided if decided else None,
                       excluded_from_validation={key: counts[key] for key in ("error", "unknown", "not_validated")},
                       measurement_groups=[groups[key] for key in sorted(groups)],
                       measurement_excluded=excluded, selection_counts=selection_counts)
    elif operation == "validate":
        matrix = {"draft_valid": {"passed": 0, "failed": 0}, "draft_invalid": {"passed": 0, "failed": 0}}
        missing_labels = 0
        for row in observations:
            if row["_display_result"] in ("passed", "failed"):
                expected = row.get("draft_expected_validation")
                if expected not in ("passed", "failed"):
                    missing_labels += 1
                    continue
                label = "draft_valid" if expected == "passed" else "draft_invalid"
                matrix[label][row["_display_result"]] += 1
        decided = sum(sum(values.values()) for values in matrix.values())
        agreed = matrix["draft_valid"]["passed"] + matrix["draft_invalid"]["failed"]
        checked.update(draft_label_confusion_matrix=matrix,
                       draft_label_agreement=agreed / decided if decided else None,
                       draft_label_agreement_denominator=decided,
                       excluded_from_agreement={"error": counts["error"], "unknown": counts["unknown"],
                                                "missing_draft_label": missing_labels})
    return checked


def load_run(path):
    """Validate the record envelope; retain incomplete runs with visible warnings."""
    raw = path.read_bytes()
    records = []
    # splitlines() would also split legal U+2028/U+2029 inside JSON strings.
    for number, line in enumerate(raw.decode("utf-8").split("\n"), 1):
        if not line.strip():
            continue
        try:
            record = json.loads(line, object_pairs_hook=unique_object,
                                parse_constant=reject_constant)
        except (ValueError, RecursionError) as exc:
            raise ReportError(f"{path.name}:{number}: invalid JSON: {exc}") from exc
        require(isinstance(record, dict), f"line {number}: record must be an object")
        records.append((number, record))
    require(bool(records), "input is empty; no run manifest or observations")
    first_line, manifest = records[0]
    require(manifest.get("record_type") == "manifest",
            f"line {first_line}: expected a run manifest, not a case dataset or raw API response")
    operation = manifest.get("operation")
    version = manifest.get("format_version")
    require(type(version) is int and (operation, version) in
            (("compile", 1), ("validate", 1), ("pipeline", 2)),
            f"unsupported run format: operation={operation!r}, format_version={version!r}")
    observations, summary, warnings, seen = [], None, [], set()
    for number, record in records[1:]:
        kind = record.get("record_type")
        require(summary is None, f"line {number}: record appears after the final summary")
        if kind == "summary":
            require(record.get("operation") == operation,
                    f"line {number}: summary operation differs from manifest")
            summary = record
        elif kind == "observation":
            require(record.get("operation", operation) == operation,
                    f"line {number}: observation operation differs from manifest")
            require(isinstance(record.get("case_id"), str) and record["case_id"].strip(),
                    f"line {number}: observation needs a nonempty case_id")
            require(isinstance(record.get("candidate_id"), str) and record["candidate_id"].strip(),
                    f"line {number}: observation needs a nonempty candidate_id")
            row = dict(record, _line=number, _warnings=[])
            if not isinstance(row.get("source"), str):
                row["_warnings"].append("原文 source 缺失或不是字符串。")
            if not isinstance(row.get("candidate"), str):
                if row.get("result") in ("passed", "failed", "api_success"):
                    row["_warnings"].append("结果声称已生成或完成验证，但实际候选 candidate 缺失。")
                elif row.get("candidate") is not None:
                    row["_warnings"].append("candidate 不是字符串。")
            if not isinstance(row.get("result"), str) or row["result"] not in RESULTS:
                row["_warnings"].append("未知或缺失的 result；保留原始值，按未知结果筛选。")
            key = (row["case_id"], row["candidate_id"])
            if key in seen:
                row["_warnings"].append("案例与候选标识重复；报告保留全部记录。")
            seen.add(key)
            check_observation(row, manifest)
            observations.append(row)
        else:
            raise ReportError(f"line {number}: unsupported record_type {kind!r}")
    if summary is None:
        warnings.append("缺少最终 summary：运行可能被中断。本页只展示已经写入的观察，不补算运行汇总。")
    elif operation != "pipeline" and (not nonnegative_int(summary.get("requests")) or summary["requests"] != len(observations)):
        warnings.append("summary.requests 与实际观察行数不一致；汇总可能不完整或已被修改。")
    elif operation == "pipeline" and (not nonnegative_int(summary.get("cases")) or summary["cases"] != len(observations)):
        warnings.append("summary.cases 与实际观察行数不一致；汇总可能不完整或已被修改。")
    if not isinstance(manifest.get("annotation_status"), str):
        warnings.append("manifest 未提供 annotation_status，不能确认案例已经人工复核。")
    if any(row["_warnings"] for row in observations):
        warnings.append("部分观察记录不完整或包含未知字段值，请查看对应案例的警示。")
    checked = checked_summary(summary, observations, operation)
    if summary is not None:
        changed = [key for key, value in checked.items() if key in summary and summary[key] != value]
        if changed:
            warnings.append("原始汇总与复核后的观察不一致：" + ", ".join(changed) + "。正文以观察复核为准，原始汇总保留在明细中。")
    return {"manifest": manifest, "operation": operation, "observations": observations,
            "summary": summary, "checked_summary": checked, "warnings": warnings, "sha256": hashlib.sha256(raw).hexdigest()}


def fields(items):
    return "<dl>" + "".join(f"<dt>{esc(label)}</dt><dd>{esc(value)}</dd>" for label, value in items) + "</dl>"


def json_details(label, value):
    # JSON is rendered as escaped text, never as executable script or raw HTML.
    text = json.dumps(value, ensure_ascii=False, indent=2, allow_nan=False)
    return f"<details><summary>{esc(label)}</summary><pre><code>{esc(text)}</code></pre></details>"


def warning_list(messages):
    if not messages:
        return ""
    return '<div class="warning"><ul>' + "".join(f"<li>{esc(item)}</li>" for item in messages) + "</ul></div>"


def measurement_label(meta):
    if not isinstance(meta, dict):
        return "计量口径未提供"
    complete = all(isinstance(meta.get(key), str) and meta[key].strip() for key in META_FIELDS)
    if not complete or meta.get("measurement_scope") != "text":
        return "计量口径不完整或范围不是 text，不能确认文本计数"
    if meta["measurement_kind"] == "estimated":
        return "估算文本 Token（记录口径）"
    if meta["measurement_kind"] == "exact":
        if (meta["measurement_encoding"].strip().casefold() == "estimate"
                or meta["measurement_source"].strip().casefold() == "heuristic-v2"):
            return "计量声明矛盾，不能确认精确文本计数"
        return "指定编码精确文本 Token（服务记录声明）"
    return "未识别的计量口径，保留原始记录"


def measurement_meta(meta):
    return fields([(label, meta.get(key)) for label, key in (
        ("计量方式", "measurement_kind"), ("来源", "measurement_source"),
        ("范围", "measurement_scope"), ("编码", "measurement_encoding"),
        ("版本", "measurement_version"))])


def render_measurement(row):
    measurement, stats = row.get("_display_measurement", row.get("measurement")), row.get("stats")
    if isinstance(measurement, dict):
        if measurement.get("status") != "included":
            result = fields([("计量汇总状态", measurement.get("status")),
                             ("未纳入原因", measurement.get("reason"))])
            if stats is not None:
                result += json_details("服务返回的计量记录（未纳入汇总）", stats)
            return result
        meta = measurement.get("group")
        original, candidate = measurement.get("original_tokens"), measurement.get("candidate_tokens")
    else:
        if not isinstance(stats, dict):
            return '<p class="muted">本记录未提供 Token 计量；报告不根据文本长度补算。</p>'
        meta = stats
        original, candidate = stats.get("original_tokens"), stats.get("compiled_tokens")
    meta = meta if isinstance(meta, dict) else {}
    result = f"<p><strong>{esc(measurement_label(meta))}</strong></p>" + measurement_meta(meta)
    if nonnegative_int(original) and nonnegative_int(candidate):
        result += f'<p class="token-count">原文 {original} → 候选 {candidate} tok</p>'
    else:
        result += warning_list(["Token 数值缺失或无效，未显示数值比较。"])
    if isinstance(measurement, dict) and nonnegative_int(measurement.get("effective_tokens")):
        result += f'<p>按规则采用/回退后的文本：{measurement["effective_tokens"]} tok（相同记录口径，未发送下游）</p>'
    if stats is not None:
        result += json_details("服务返回的计量元数据", stats)
    return result


def render_stage(name, stage, *, http=False):
    if not isinstance(stage, dict):
        return warning_list([f"{name} 阶段记录无效。"])
    status = stage.get("result" if http else "status")
    label = STAGE_STATUSES.get(status, plain(status)) if isinstance(status, str) else plain(status)
    items = [("状态", label)]
    if http:
        items.extend([("HTTP", stage.get("http_status")),
                      ("耗时", duration(stage.get("elapsed_seconds")))])
    else:
        items.extend([("模型（记录值）", stage.get("model")),
                      ("政策版本", stage.get("policy_version")),
                      ("耗时", duration(stage.get("duration_ms"), "ms"))])
    if stage.get("skip_reason") is not None:
        items.append(("跳过原因", stage["skip_reason"]))
    title = STAGE_NAMES.get(name, name) if isinstance(name, str) else plain(name)
    result = f'<section class="stage"><h4>{esc(title)}</h4>' + fields(items)
    if stage.get("issues"):
        result += json_details("阶段问题", stage["issues"])
    if stage.get("issues_omitted"):
        result += '<p class="note">运行器已省略阶段异常详情；报告没有补回上游原始响应。</p>'
    if "reading" in stage:
        result += json_details("阅读器结构化理解 · reading", stage["reading"])
    return result + "</section>"


def render_validation(row):
    result = ""
    http_stages = row.get("stages")
    if isinstance(http_stages, dict):
        result += "<h3>请求阶段</h3>"
        for name, stage in http_stages.items():
            result += render_stage(name, stage, http=True)
    validation = row.get("validation")
    if not isinstance(validation, dict):
        return result + '<p class="muted">未提供验证回执。</p>'
    result += "<h3>验证回执</h3>" + fields([(label, validation.get(key)) for label, key in (
        ("请求模式", "requested_mode"), ("回执检查", "receipt_check"),
        ("记录状态", "status"), ("执行方法", "method"), ("政策版本", "policy_version"))])
    issues = validation.get("issues")
    if isinstance(issues, list):
        result += "<h4>记录中的问题</h4>"
        result += ("<ul>" + "".join(f"<li>{esc(issue)}</li>" for issue in issues) + "</ul>") if issues else '<p class="muted">问题列表为空；这不证明语义完整。</p>'
    if validation.get("issues_omitted"):
        result += '<p class="note">运行器已省略异常详情；状态仍保留为异常或未知。</p>'
    stages = validation.get("stages")
    if isinstance(stages, list):
        for stage in stages:
            result += render_stage(stage.get("name", "未知阶段"), stage) if isinstance(stage, dict) else warning_list(["验证阶段记录不是对象。"])
    elif stages is not None:
        result += warning_list(["validation.stages 不是数组，未尝试推断阶段结果。"])
    return result


def render_case(row, number, operation):
    category = row["_display_result"]
    label = RESULTS[category]
    result = f'<article class="case" data-case="{number}" data-result="{category}">'
    result += f'<div class="case-head"><h2>{esc(row["case_id"])} / {esc(row["candidate_id"])}</h2><span class="badge result-{category}">{esc(label)}</span></div>'
    result += warning_list(row["_warnings"])
    result += fields([("原始结果字段", row.get("result")), ("JSONL 行号", row["_line"]),
                      ("HTTP", row.get("http_status")), ("观察耗时", duration(row.get("elapsed_seconds")))])
    selection = row.get("_display_selection", row.get("selection"))
    if isinstance(selection, dict):
        output = selection.get("output")
        choice = {"candidate": "候选", "source": "回退原文"}.get(output, "未识别的输出选择") if isinstance(output, str) else "未识别的输出选择"
        result += fields([("按评测规则选择（未发送下游）", choice), ("选择原因", selection.get("reason"))])
    if operation == "pipeline" and (row.get("selection") != row.get("_display_selection")
                                   or row.get("measurement") != row.get("_display_measurement")):
        result += json_details("原始计量与输出选择记录（正文使用复核结果）",
                               {"measurement": row.get("measurement"), "selection": row.get("selection")})
    candidate_label = "提交验证的既有候选" if operation == "validate" else "本次生成的实际候选"
    result += '<div class="columns">'
    for role, title in (("source", "原文"), ("candidate", candidate_label)):
        text = row.get(role)
        result += f"<section><h3>{title}</h3>"
        # The code child preserves an initial newline which bare <pre> drops.
        result += (f'<pre><code data-text-role="{role}">{esc(text)}</code></pre>' if isinstance(text, str)
                   else '<p class="note">文本未生成或未记录。</p>')
        result += "</section>"
    result += "</div><h3>计数依据</h3>" + render_measurement(row) + render_validation(row)
    if "draft_expected_validation" in row:
        result += fields([("草拟标签的期望结果", row["draft_expected_validation"])])
    for key, label in (("literal_presence_check", "保护字面量出现检查 · 不代表语义保留"),
                       ("expected_relations", "案例中的关系参考 · 请结合标注状态审阅"),
                       ("protected_literals", "保护字面量"), ("usage", "服务记录的 usage · 不推导账单节省")):
        if key in row:
            result += json_details(label, row[key])
    return result + "</article>"


def render_summary(summary, operation, observation_count):
    if summary is None:
        return '<p class="note">未提供最终汇总。</p>'
    result = fields([(label, summary.get(key)) for label, key in (
        ("记录的 HTTP 请求数", "requests"), ("计划请求上限", "planned_requests"),
        ("因预算未执行", "skipped_by_budget"), ("API 异常", "error"), ("未知结果", "unknown"))])
    rate_key, denominator_key = ("validation_pass_rate", "validation_decided") if operation == "pipeline" else ("draft_label_agreement", "draft_label_agreement_denominator")
    if rate_key in summary:
        rate, denominator = summary[rate_key], summary.get(denominator_key)
        label = "所选验证流程通过率" if operation == "pipeline" else "草拟标签一致比例"
        valid = observation_count > 0 and type(rate) in (int, float) and math.isfinite(rate) and 0 <= rate <= 1 and nonnegative_int(denominator) and denominator > 0
        display = f"{rate * 100:.1f}%（记录分母 {denominator}）" if valid else "未建立（没有有效的已决分母或比例）"
        result += fields([(label, display)])
        result += '<p class="note">此比例来自运行记录，不能作为语义准确率、下游行为成功率或费用指标。草拟标签也不等于独立真值。</p>'
    for key, label in (("excluded_from_validation", "排除在流程通过率之外的记录"),
                       ("excluded_from_agreement", "排除在标签一致比例之外的记录"),
                       ("draft_label_confusion_matrix", "草拟标签与流程结果交叉记录"),
                       ("measurement_excluded", "未纳入计数汇总的原因")):
        if key in summary:
            result += json_details(label, summary[key])
    if isinstance(summary.get("selection_counts"), dict):
        result += fields([("按规则采用候选数（未发送下游）", summary["selection_counts"].get("candidate")),
                          ("按规则回退原文数", summary["selection_counts"].get("source"))])
    groups = summary.get("measurement_groups")
    if isinstance(groups, list):
        result += '<h3>文本计数分组（按观察复核）</h3><p class="muted">不同编码、来源、版本及估算/精确口径分别展示，不合并 Token 总量。采用/回退是评测规则推导，未发送下游。</p>'
        for group in groups:
            if not isinstance(group, dict):
                result += warning_list(["计量分组不是对象，未展示为有效分组。"])
                continue
            result += '<section class="measurement-group"><h4>' + esc(measurement_label(group)) + "</h4>" + measurement_meta(group)
            result += fields([(label, group.get(key)) for label, key in (
                ("案例数", "cases"), ("原文文本 Token", "original_tokens"),
                ("候选文本 Token", "candidate_tokens"), ("按规则采用/回退后的文本 Token", "effective_tokens"),
                ("候选 Token 变化（正数增加）", "candidate_delta_tokens"),
                ("按规则回退后减少的文本 Token", "effective_savings_tokens"),
                ("按规则采用候选数", "adopted_candidates"), ("按规则回退原文数", "fallback_to_source"))])
            changes = group.get("candidate_changes")
            if isinstance(changes, dict):
                result += fields([(label, changes.get(key)) for label, key in (
                    ("候选缩小案例", "reduced"), ("候选不变案例", "unchanged"), ("候选变大案例", "expanded"))])
            result += "</section>"
    elif groups is not None:
        result += warning_list(["measurement_groups 不是数组，未汇总文本计数。"])
    return result


def build_html(run, input_name):
    manifest, observations, summary = run["manifest"], run["observations"], run["summary"]
    annotation = manifest.get("annotation_status")
    annotation_label = "助手草拟，待人工复核" if annotation == "assistant_drafted_requires_human_review" else plain(annotation)
    reader = manifest.get("reader_profile")
    reader_label = "继承所选方案" if reader is None else "与验证方案相同" if reader == "" else reader
    state = "缺少最终汇总 · 运行记录不完整" if summary is None else "已记录最终汇总"
    if run["warnings"] and summary is not None:
        state += " · 存在记录警示"
    content = f"<header><h1>Babel 离线评测报告</h1><p>{esc(input_name)}</p>"
    content += '<p class="muted">本页使用运行器的同一套规则复核已有回执和计量记录，不调用模型、不重新分词、不复核语义。校验通过只表示所选流程的记录结果。</p>'
    content += fields([("运行记录状态", state), ("操作", run["operation"]),
        ("案例标注状态", annotation_label), ("标注状态原值", annotation),
        ("开始时间（记录值）", manifest.get("started_at_utc")), ("编译/主要方案", manifest.get("profile")),
        ("验证方案", manifest.get("validation_profile", manifest.get("profile"))),
        ("阅读器方案", reader_label), ("验证模式", manifest.get("validation_mode")),
        ("输出预设", manifest.get("output_preset")),
        ("请求编码", manifest.get("token_encoding_requested") or "继承所选方案"),
        ("输出选择范围", "仅评测规则，未发送下游" if run["operation"] == "pipeline" else "未评估下游选择"),
        ("数据集 SHA-256", manifest.get("dataset_sha256")),
        ("上游模型身份", manifest.get("upstream_model_identity", "未独立核验"))])
    content += warning_list(run["warnings"]) + "</header>"
    content += '<section class="overview"><h2>观察复核汇总</h2><p class="muted">通过率与计数分组来自复核后的观察；原始 summary 不覆盖观察结果。HTTP 请求/预算数量沿用运行记录。</p>'
    content += render_summary(run["checked_summary"], run["operation"], len(observations))
    if summary is not None:
        content += json_details("原始运行器汇总（保留供核查）", {key: value for key, value in summary.items() if key != "endpoint"})
    content += "</section>"
    counts = Counter(row["_display_result"] for row in observations)
    content += '<section class="overview"><h2>观察记录</h2><p>以下数量按本文件中的观察行计数，不代表 HTTP 请求数或上游模型调用数。</p>'
    content += fields([(RESULTS[key], count) for key, count in counts.items()])
    content += '<div class="filters"><label>结果 <select id="result-filter"><option value="all">全部</option>'
    content += "".join(f'<option value="{key}">{esc(label)}</option>' for key, label in RESULTS.items())
    content += '</select></label><label>搜索 <input id="search" type="search" placeholder="案例、文本、问题或模型" autocomplete="off"></label>'
    content += f'<span id="visible-count" role="status">显示 {len(observations)} / {len(observations)} 条观察</span></div>'
    content += '<noscript><p>JavaScript 未启用，当前展示全部记录；筛选和搜索不可用。</p></noscript></section>'
    if not observations:
        content += '<section class="overview" id="empty-results"><h2>没有观察记录</h2><p>本文件没有实际候选或验证结果。未生成通过率、语义分数或文本计数。</p></section>'
    content += '<p id="no-matches" class="note" hidden>没有符合当前筛选条件的记录。</p>'
    content += "".join(render_case(row, i, run["operation"]) for i, row in enumerate(observations, 1))
    content += f'<footer>输入文件 SHA-256：<code>{run["sha256"]}</code><br>所有内容来自输入记录；运行配置的 endpoint 未展示。报告无外部资源。</footer>'
    script_hash = base64.b64encode(hashlib.sha256(SCRIPT.encode("utf-8")).digest()).decode("ascii")
    policy = f"default-src 'none'; script-src 'sha256-{script_hash}'; style-src 'unsafe-inline'; connect-src 'none'; base-uri 'none'; form-action 'none'"
    return ('<!doctype html>\n<html lang="zh-CN"><head><meta charset="utf-8">'
            '<meta name="viewport" content="width=device-width, initial-scale=1">'
            f'<meta http-equiv="Content-Security-Policy" content="{esc(policy)}">'
            f'<title>Babel 评测报告 · {esc(input_name)}</title><style>{CSS}</style></head>'
            f'<body>{content}<script>{SCRIPT}</script></body></html>\n')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path, help="existing Babel JSONL run")
    parser.add_argument("--output", type=Path, required=True, help="new HTML report; existing files are never overwritten")
    args = parser.parse_args(argv)
    try:
        run = load_run(args.input)
        rendered = build_html(run, args.input.name)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        with args.output.open("x", encoding="utf-8", newline="\n") as handle:
            handle.write(rendered)
        print(f"Report written: {args.output} ({len(run['observations'])} observations)")
        for warning in run["warnings"]:
            print(f"report warning: {warning}", file=sys.stderr)
        return 0
    except (ReportError, OSError, UnicodeDecodeError, RecursionError) as exc:
        print(f"report error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
