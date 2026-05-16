"""Analyzer: turn probe.py output into a human-readable Markdown report.

Reads:
  - <stem>.jsonl  : one CallRecord per line
  - <stem>.json   : mode-level summary written by probe.py

Writes:
  - <stem>.md     : Markdown report with overall stats, sliding-window
                    RPM/TPM, error breakdown, and mode-specific tables.

Can be invoked standalone:
  python analyzer.py results/<stem>.jsonl results/<stem>.json [out.md]
"""

from __future__ import annotations

import json
import statistics
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any


def _load_jsonl(p: Path) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    with p.open(encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                out.append(json.loads(line))
    return out


def _pct(vals: list[float], p: int):
    if not vals:
        return None
    vs = sorted(vals)
    idx = max(0, min(len(vs) - 1, int(round((p / 100.0) * (len(vs) - 1)))))
    return round(vs[idx], 3)


def _classify_error(rec: dict[str, Any]) -> str:
    """Classify a record into one of: ok / 429:RPM / 429:TPM / 429:burst /
    429:concurrency / 429:other / 5xx:server / network / 4xx:other.

    DashScope uses a few distinguishable signatures:
      - errorType "THROTTLING.userQPSLimit"  -> burst / QPS
      - message contains "rate limit" / "exceeded your current requests" -> RPM
      - message contains "quota" / "tokens" (but NOT "concurrency")     -> TPM
      - message "concurrency allocated quota exceeded"                   -> concurrency
      - message "Request rate increased too quickly"                     -> burst
    Heuristic — falls back to generic 429:other when ambiguous.
    """
    if rec.get("ok"):
        return "ok"
    status = rec.get("http_status")
    err = (rec.get("error_type") or "").lower()
    msg = (rec.get("error_message") or "").lower()
    if status == 429:
        if "concurrency allocated quota" in msg:
            return "429:concurrency (instant reject)"
        if "qps" in err or "increased too quickly" in msg or "rate increased" in msg:
            return "429:burst (QPS / rate-spike)"
        if "quota" in err or "quota" in msg or "tokens" in msg or "tpm" in err:
            return "429:TPM (token quota)"
        if (
            "rate" in err
            or "rpm" in err
            or "requests" in msg
            or "current requests list" in msg
        ):
            return "429:RPM (requests/min)"
        return f"429:other ({err or '?'})"
    if status and status >= 500:
        return f"{status}:server"
    if not status:
        return f"network:{err or '?'}"
    return f"{status}:{err or '?'}"


def _seconds_buckets(recs: list[dict[str, Any]]) -> dict[int, dict[str, int]]:
    """Bucket records into 1-second buckets keyed on start_ts (relative)."""
    if not recs:
        return {}
    base = min(r["start_ts"] for r in recs)
    buckets: dict[int, dict[str, int]] = defaultdict(
        lambda: {"req": 0, "ok": 0, "fail": 0, "tokens": 0}
    )
    for r in recs:
        b = int(r["start_ts"] - base)
        buckets[b]["req"] += 1
        if r.get("ok"):
            buckets[b]["ok"] += 1
            buckets[b]["tokens"] += r.get("total_tokens") or 0
        else:
            buckets[b]["fail"] += 1
    return buckets


def _windowed(buckets: dict[int, dict[str, int]], window: int) -> list[dict[str, int]]:
    """Sliding `window`-second sums anchored at each second's right edge."""
    if not buckets:
        return []
    last = max(buckets.keys())
    out: list[dict[str, int]] = []
    for end in range(window, last + 1):
        win = {"end_s": end, "req": 0, "ok": 0, "fail": 0, "tokens": 0}
        for t in range(end - window, end):
            b = buckets.get(t)
            if b:
                for k in ("req", "ok", "fail", "tokens"):
                    win[k] += b[k]
        out.append(win)
    return out


def write_markdown_report(raw_jsonl: Path, summary_json: Path, out_md: Path) -> None:
    recs = _load_jsonl(raw_jsonl)
    summary = json.loads(summary_json.read_text(encoding="utf-8"))

    total = len(recs)
    ok = sum(1 for r in recs if r.get("ok"))
    fail = total - ok
    cat = Counter(_classify_error(r) for r in recs)
    latencies = [r["latency_s"] for r in recs if r.get("ok")]
    tokens = [r.get("total_tokens") for r in recs if r.get("ok") and r.get("total_tokens")]

    buckets = _seconds_buckets(recs)
    win60 = _windowed(buckets, 60)
    peak_rpm = max((w["req"] for w in win60), default=0)
    peak_ok_rpm = max((w["ok"] for w in win60), default=0)
    peak_tpm = max((w["tokens"] for w in win60), default=0)
    peak_rps = max((b["req"] for b in buckets.values()), default=0)

    lines: list[str] = []
    lines.append(
        f"# Qwen rate-limit probe — `{summary.get('mode')}` on `{summary.get('model')}`"
    )
    lines.append("")
    lines.append(f"- Timestamp (UTC): `{summary.get('timestamp_utc')}`")
    lines.append(f"- Base URL: `{summary.get('base_url')}`")
    lines.append(f"- Raw data: `{summary.get('raw_jsonl')}`")
    lines.append("")
    lines.append("## Overall")
    lines.append("")
    lines.append("| metric | value |")
    lines.append("|--------|------:|")
    lines.append(f"| total requests | {total} |")
    lines.append(f"| ok | {ok} |")
    lines.append(f"| fail | {fail} |")
    lines.append(f"| success rate | {(ok / total * 100 if total else 0):.1f}% |")
    if latencies:
        lines.append(
            f"| latency p50 / p95 / p99 | "
            f"{_pct(latencies, 50)}s / {_pct(latencies, 95)}s / {_pct(latencies, 99)}s |"
        )
    if tokens:
        lines.append(f"| avg total_tokens / ok req | {statistics.mean(tokens):.0f} |")
    lines.append(f"| peak 1s RPS | {peak_rps} |")
    lines.append(f"| peak 60s RPM (issued) | {peak_rpm} |")
    lines.append(f"| peak 60s RPM (ok) | {peak_ok_rpm} |")
    lines.append(f"| peak 60s TPM (ok) | {peak_tpm} |")
    lines.append("")

    lines.append("## Error breakdown")
    lines.append("")
    lines.append("| category | count | share |")
    lines.append("|----------|------:|------:|")
    for k, c in cat.most_common():
        lines.append(f"| {k} | {c} | {c / total * 100:.1f}% |")
    lines.append("")

    mode = summary.get("mode")
    if mode == "concurrency":
        lines.append("## Per-concurrency-level results")
        lines.append("")
        lines.append(
            "| concurrency | ok / total | success% | wall (s) | p50 (s) | p95 (s) | error mix |"
        )
        lines.append(
            "|------------:|-----------:|---------:|--------:|--------:|--------:|----------|"
        )
        for lv in summary.get("levels", []):
            sr = lv["ok"] / lv["total"] * 100 if lv["total"] else 0
            errs = ", ".join(f"{k}×{v}" for k, v in lv.get("errors", {}).items()) or "-"
            lines.append(
                f"| {lv['concurrency']} | {lv['ok']}/{lv['total']} | {sr:.1f}% "
                f"| {lv['wall_s']} | {lv['p50_latency_s']} | {lv['p95_latency_s']} | {errs} |"
            )
        lines.append("")
        first_bad = next(
            (
                lv["concurrency"]
                for lv in summary.get("levels", [])
                if any("429" in k for k in lv.get("errors", {}))
            ),
            None,
        )
        if first_bad is not None:
            lines.append(
                f"**Heuristic concurrency ceiling**: first 429 observed at concurrency = **{first_bad}**."
            )
            lines.append("")

    if mode in ("rpm", "tpm"):
        lines.append("## Rolling 60-second window (sampled every 10s)")
        lines.append("")
        lines.append("| t_end (s) | req | ok | fail | tokens |")
        lines.append("|----------:|----:|---:|-----:|-------:|")
        sampled = [w for w in win60 if w["end_s"] % 10 == 0]
        for w in sampled[:40]:
            lines.append(
                f"| {w['end_s']} | {w['req']} | {w['ok']} | {w['fail']} | {w['tokens']} |"
            )
        lines.append("")
        # Try to back out the ceiling: take max ok in any 60s window before
        # the first sustained failure window.
        first_fail_win = next((w for w in win60 if w["fail"] > 0), None)
        if first_fail_win:
            lines.append(
                f"**First 60s window with failures**: ends at t+{first_fail_win['end_s']}s "
                f"(req={first_fail_win['req']}, ok={first_fail_win['ok']}, "
                f"tokens={first_fail_win['tokens']})."
            )
            lines.append(
                "Use this as the practical ceiling: just below this issued RPM / TPM "
                "the success rate should be ~100%."
            )
            lines.append("")

    if mode == "recovery":
        lines.append("## Recovery probes")
        lines.append("")
        lines.append("| t after burst (s) | status | ok | error_type |")
        lines.append("|------------------:|-------:|:--:|-----------|")
        for p in summary.get("probes", []):
            lines.append(
                f"| {p['elapsed_s']} | {p['http_status']} | "
                f"{'ok' if p['ok'] else 'fail'} | {p.get('error_type') or ''} |"
            )
        lines.append("")
        first_ok = next((p["elapsed_s"] for p in summary.get("probes", []) if p["ok"]), None)
        if first_ok is not None:
            lines.append(f"**Recovery time**: first ok at **t+{first_ok}s**.")
            lines.append("")

    failures = [r for r in recs if not r.get("ok")]
    if failures:
        lines.append("## First 5 failure samples")
        lines.append("")
        for r in failures[:5]:
            lines.append("```json")
            lines.append(
                json.dumps(
                    {
                        "seq": r.get("seq"),
                        "http_status": r.get("http_status"),
                        "error_type": r.get("error_type"),
                        "error_message": (r.get("error_message") or "")[:200],
                        "retry_after_s": r.get("retry_after_s"),
                        "rate_headers": r.get("rate_headers"),
                    },
                    ensure_ascii=False,
                )
            )
            lines.append("```")
        lines.append("")

    out_md.write_text("\n".join(lines), encoding="utf-8")


if __name__ == "__main__":
    import sys

    raw = Path(sys.argv[1])
    smry = Path(sys.argv[2])
    out = Path(sys.argv[3]) if len(sys.argv) > 3 else raw.with_suffix(".md")
    write_markdown_report(raw, smry, out)
    print(out)
