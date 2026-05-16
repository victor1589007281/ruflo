#!/usr/bin/env python3
"""Qwen DashScope rate-limit probe.

Probes the DashScope (Qwen) API across multiple rate-limit dimensions so
you can figure out, for *your* account, where the ceilings actually are:

  - concurrency : step concurrency 1 -> N, look for the first 429
  - rpm         : hold a constant Requests-Per-Minute load for N seconds
  - tpm         : push big prompts at moderate RPM to saturate TPM first
  - burst       : fire a single shot of K requests, observe burst guard
  - recovery    : trigger 429, then probe at intervals to find recovery time

All raw call records are written to results/<model>_<mode>_<ts>.jsonl.
A JSON summary and a Markdown report are written alongside.

Default model is `qwen-plus` (DashScope name closest to "Qwen 3.6 Plus").
Override via --model or env QWEN_MODEL.

Usage:
  export DASHSCOPE_API_KEY=sk-...
  python probe.py concurrency --model qwen-plus --max-concurrency 64
  python probe.py rpm --model qwen-plus --target-rpm 300 --duration 90
  python probe.py tpm --model qwen-plus --tokens-per-request 4000 --target-rpm 60 --duration 120
  python probe.py burst --model qwen-plus --burst-size 200
  python probe.py recovery --model qwen-plus --trigger-burst 150
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Optional

import httpx

# Two known DashScope endpoints:
#   - OpenAI-compatible: https://dashscope.aliyuncs.com/compatible-mode/v1
#   - Anthropic-compatible (coding proxy): https://coding.dashscope.aliyuncs.com/apps/anthropic/v1
# claude-go uses the anthropic-compatible one by default with model id
# `qwen3.6-plus`, so that's our default here.
DEFAULT_BASE_URL = os.environ.get(
    "DASHSCOPE_BASE_URL",
    "https://coding.dashscope.aliyuncs.com/apps/anthropic/v1",
)
DEFAULT_MODEL = os.environ.get("QWEN_MODEL", "qwen3.6-plus")
DEFAULT_PROTOCOL = os.environ.get("QWEN_PROTOCOL", "anthropic")  # anthropic | openai
RESULTS_DIR = Path(__file__).parent / "results"
RESULTS_DIR.mkdir(exist_ok=True)


# ---------- record ----------

@dataclass
class CallRecord:
    seq: int
    start_ts: float
    end_ts: float
    latency_s: float
    http_status: Optional[int]
    ok: bool
    error_type: Optional[str] = None
    error_message: Optional[str] = None
    request_id: Optional[str] = None
    prompt_tokens: Optional[int] = None
    completion_tokens: Optional[int] = None
    total_tokens: Optional[int] = None
    retry_after_s: Optional[float] = None
    rate_headers: dict[str, str] = field(default_factory=dict)

    def to_jsonl(self) -> str:
        return json.dumps(asdict(self), ensure_ascii=False)


# ---------- caller ----------

class QwenCaller:
    """Minimal client for DashScope. Supports OpenAI- or Anthropic-compatible
    endpoints — pick via `protocol`."""

    def __init__(
        self,
        api_key: str,
        base_url: str = DEFAULT_BASE_URL,
        model: str = DEFAULT_MODEL,
        protocol: str = DEFAULT_PROTOCOL,
        timeout: float = 60.0,
    ) -> None:
        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout = timeout
        self.protocol = protocol
        if protocol == "anthropic":
            headers = {
                "x-api-key": api_key,
                "anthropic-version": "2023-06-01",
                "content-type": "application/json",
            }
        elif protocol == "openai":
            headers = {
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
            }
        else:
            raise ValueError(f"unknown protocol: {protocol}")
        self.client = httpx.AsyncClient(
            timeout=timeout,
            limits=httpx.Limits(max_connections=512, max_keepalive_connections=128),
            headers=headers,
        )

    async def close(self) -> None:
        await self.client.aclose()

    async def chat(
        self,
        seq: int,
        prompt: str,
        max_tokens: int = 64,
        temperature: float = 0.2,
    ) -> CallRecord:
        if self.protocol == "anthropic":
            url = f"{self.base_url}/messages"
            payload: dict[str, Any] = {
                "model": self.model,
                "max_tokens": max_tokens,
                "temperature": temperature,
                "messages": [{"role": "user", "content": prompt}],
                "stream": False,
            }
        else:
            url = f"{self.base_url}/chat/completions"
            payload = {
                "model": self.model,
                "messages": [{"role": "user", "content": prompt}],
                "max_tokens": max_tokens,
                "temperature": temperature,
                "stream": False,
            }
        rec = CallRecord(
            seq=seq,
            start_ts=time.time(),
            end_ts=0.0,
            latency_s=0.0,
            http_status=None,
            ok=False,
        )
        try:
            resp = await self.client.post(url, json=payload)
            rec.end_ts = time.time()
            rec.latency_s = rec.end_ts - rec.start_ts
            rec.http_status = resp.status_code
            # Capture any header that looks rate-limit-related so we can
            # data-mine it post-hoc (DashScope sometimes adds X-Request-Id,
            # vendors like OpenAI add X-RateLimit-Remaining-*).
            rec.rate_headers = {
                k: v for k, v in resp.headers.items()
                if any(s in k.lower() for s in ("ratelimit", "request-id", "retry"))
            }
            rid = resp.headers.get("x-request-id") or resp.headers.get("X-Request-Id")
            if rid:
                rec.request_id = rid
            ra = resp.headers.get("retry-after")
            if ra:
                try:
                    rec.retry_after_s = float(ra)
                except ValueError:
                    pass
            if resp.status_code == 200:
                data = resp.json()
                rec.ok = True
                if self.protocol == "anthropic":
                    usage = data.get("usage") or {}
                    rec.prompt_tokens = usage.get("input_tokens")
                    rec.completion_tokens = usage.get("output_tokens")
                    rec.total_tokens = (
                        (usage.get("input_tokens") or 0)
                        + (usage.get("output_tokens") or 0)
                        + (usage.get("cache_creation_input_tokens") or 0)
                    )
                    if not rec.request_id:
                        rec.request_id = data.get("id")
                else:
                    usage = data.get("usage") or {}
                    rec.prompt_tokens = usage.get("prompt_tokens")
                    rec.completion_tokens = usage.get("completion_tokens")
                    rec.total_tokens = usage.get("total_tokens")
            else:
                try:
                    body = resp.json()
                    err = body.get("error") or body
                    rec.error_type = (
                        err.get("type")
                        or err.get("errorType")
                        or err.get("code")
                    )
                    rec.error_message = err.get("message") or json.dumps(err)[:300]
                except Exception:
                    rec.error_message = resp.text[:300]
        except httpx.TimeoutException as e:
            rec.end_ts = time.time()
            rec.latency_s = rec.end_ts - rec.start_ts
            rec.error_type = "client.timeout"
            rec.error_message = str(e)
        except Exception as e:  # noqa: BLE001
            rec.end_ts = time.time()
            rec.latency_s = rec.end_ts - rec.start_ts
            rec.error_type = "client.exception"
            rec.error_message = repr(e)[:300]
        return rec


# ---------- prompts ----------

SHORT_PROMPT = "请用一个字回答：天空是什么颜色？"


def long_prompt(tokens_target: int) -> str:
    """Build a prompt sized to roughly `tokens_target` Qwen tokens.

    Heuristic: Qwen tokenizer averages ~1 token per 1.5 Chinese characters,
    so we generate ~1.5 * tokens_target characters.
    """
    chunk = "请总结这段文字。" + ("分析问题需要充分调研，并且持续优化方案。" * 50)
    target_chars = max(int(tokens_target * 1.5), 200)
    out = ""
    while len(out) < target_chars:
        out += chunk
    return out[:target_chars] + "\n请用一段话总结上文。"


# ---------- modes ----------

async def run_concurrency(args, caller: QwenCaller, sink) -> dict[str, Any]:
    """Step concurrency from 1..N (geometric), send a batch at each level."""
    levels: list[int] = []
    lv = 1
    while lv <= args.max_concurrency:
        levels.append(lv)
        lv *= 2
    if levels[-1] != args.max_concurrency:
        levels.append(args.max_concurrency)

    per_level: list[dict[str, Any]] = []
    seq = 0
    for L in levels:
        async def one(i: int) -> CallRecord:
            return await caller.chat(seq + i, SHORT_PROMPT, max_tokens=16)

        t0 = time.time()
        results = await asyncio.gather(*[one(i) for i in range(L)])
        wall = time.time() - t0
        for r in results:
            sink(r)
        seq += L

        ok = sum(1 for r in results if r.ok)
        err_types: dict[str, int] = {}
        for r in results:
            if not r.ok:
                key = f"{r.http_status}:{r.error_type or '?'}"
                err_types[key] = err_types.get(key, 0) + 1
        per_level.append({
            "concurrency": L,
            "total": L,
            "ok": ok,
            "fail": L - ok,
            "wall_s": round(wall, 3),
            "p50_latency_s": _pct([r.latency_s for r in results if r.ok], 50),
            "p95_latency_s": _pct([r.latency_s for r in results if r.ok], 95),
            "errors": err_types,
        })
        print(
            f"[concurrency={L:4}] ok={ok}/{L}  wall={wall:.2f}s  errors={err_types}",
            flush=True,
        )
        # cool down so the next level starts in a fresh RPM/TPM window
        await asyncio.sleep(args.cool_down)

    return {"mode": "concurrency", "levels": per_level}


async def run_rpm(args, caller: QwenCaller, sink) -> dict[str, Any]:
    """Maintain a fixed RPM via fire-and-forget pacing; observe failures."""
    interval = 60.0 / args.target_rpm
    deadline = time.time() + args.duration
    seq = 0
    in_flight: set[asyncio.Task] = set()

    async def fire(i: int) -> None:
        r = await caller.chat(i, SHORT_PROMPT, max_tokens=16)
        sink(r)

    next_t = time.time()
    while time.time() < deadline:
        now = time.time()
        if now < next_t:
            await asyncio.sleep(next_t - now)
        t = asyncio.create_task(fire(seq))
        in_flight.add(t)
        t.add_done_callback(in_flight.discard)
        seq += 1
        next_t += interval
    if in_flight:
        await asyncio.gather(*in_flight, return_exceptions=True)
    print(
        f"[rpm] fired={seq} duration={args.duration}s target_rpm={args.target_rpm}",
        flush=True,
    )
    return {
        "mode": "rpm",
        "fired": seq,
        "target_rpm": args.target_rpm,
        "duration_s": args.duration,
    }


async def run_tpm(args, caller: QwenCaller, sink) -> dict[str, Any]:
    """Push large prompts at moderate RPM to saturate TPM before RPM."""
    interval = 60.0 / args.target_rpm
    deadline = time.time() + args.duration
    seq = 0
    in_flight: set[asyncio.Task] = set()
    prompt = long_prompt(args.tokens_per_request)

    async def fire(i: int) -> None:
        r = await caller.chat(i, prompt, max_tokens=args.completion_tokens)
        sink(r)

    next_t = time.time()
    while time.time() < deadline:
        now = time.time()
        if now < next_t:
            await asyncio.sleep(next_t - now)
        t = asyncio.create_task(fire(seq))
        in_flight.add(t)
        t.add_done_callback(in_flight.discard)
        seq += 1
        next_t += interval
    if in_flight:
        await asyncio.gather(*in_flight, return_exceptions=True)
    print(f"[tpm] fired={seq} tokens_per_request~{args.tokens_per_request}", flush=True)
    return {
        "mode": "tpm",
        "fired": seq,
        "target_rpm": args.target_rpm,
        "duration_s": args.duration,
        "tokens_per_request": args.tokens_per_request,
        "completion_tokens": args.completion_tokens,
    }


async def run_burst(args, caller: QwenCaller, sink) -> dict[str, Any]:
    """One-shot burst of K requests in parallel."""
    async def one(i: int) -> CallRecord:
        return await caller.chat(i, SHORT_PROMPT, max_tokens=16)

    t0 = time.time()
    results = await asyncio.gather(*[one(i) for i in range(args.burst_size)])
    wall = time.time() - t0
    for r in results:
        sink(r)
    ok = sum(1 for r in results if r.ok)
    err_types: dict[str, int] = {}
    for r in results:
        if not r.ok:
            key = f"{r.http_status}:{r.error_type or '?'}"
            err_types[key] = err_types.get(key, 0) + 1
    print(
        f"[burst] {args.burst_size} req in {wall:.2f}s  ok={ok}  errors={err_types}",
        flush=True,
    )
    return {
        "mode": "burst",
        "burst_size": args.burst_size,
        "wall_s": round(wall, 3),
        "ok": ok,
        "errors": err_types,
    }


async def run_recovery(args, caller: QwenCaller, sink) -> dict[str, Any]:
    """Trigger 429 with a burst, then probe recovery at fixed intervals."""
    burst = args.trigger_burst
    print(f"[recovery] firing burst of {burst} to trigger 429...", flush=True)
    results = await asyncio.gather(
        *[caller.chat(i, SHORT_PROMPT, max_tokens=8) for i in range(burst)]
    )
    for r in results:
        sink(r)
    saw_429 = any(r.http_status == 429 for r in results)
    if not saw_429:
        print(
            "[recovery] WARN: no 429 observed; increase --trigger-burst or pick a smaller-quota model",
            flush=True,
        )

    probes: list[dict[str, Any]] = []
    elapsed = 0.0
    seq = burst
    while elapsed < args.max_wait:
        await asyncio.sleep(args.probe_step)
        elapsed += args.probe_step
        r = await caller.chat(seq, SHORT_PROMPT, max_tokens=8)
        seq += 1
        sink(r)
        probes.append({
            "elapsed_s": round(elapsed, 1),
            "http_status": r.http_status,
            "ok": r.ok,
            "error_type": r.error_type,
        })
        print(
            f"[recovery] t+{elapsed:5.1f}s status={r.http_status} ok={r.ok} err={r.error_type}",
            flush=True,
        )
        if r.ok:
            break
    return {"mode": "recovery", "trigger_burst": burst, "saw_429": saw_429, "probes": probes}


def _pct(vals: list[float], p: int) -> Optional[float]:
    if not vals:
        return None
    vs = sorted(vals)
    idx = max(0, min(len(vs) - 1, int(round((p / 100.0) * (len(vs) - 1)))))
    return round(vs[idx], 3)


# ---------- driver ----------

async def main_async(args) -> None:
    api_key = os.environ.get("DASHSCOPE_API_KEY")
    if not api_key:
        sys.exit("error: DASHSCOPE_API_KEY env var is required")

    caller = QwenCaller(
        api_key=api_key,
        base_url=args.base_url,
        model=args.model,
        protocol=args.protocol,
    )
    ts = datetime.now(timezone.utc).strftime("%Y%m%d_%H%M%S")
    stem = f"{args.model.replace('/', '_')}_{args.mode}_{ts}"
    raw_path = RESULTS_DIR / f"{stem}.jsonl"
    sum_path = RESULTS_DIR / f"{stem}.json"
    md_path = RESULTS_DIR / f"{stem}.md"

    raw_fp = raw_path.open("w", encoding="utf-8")

    def sink(rec: CallRecord) -> None:
        raw_fp.write(rec.to_jsonl() + "\n")
        raw_fp.flush()

    runners = {
        "concurrency": run_concurrency,
        "rpm": run_rpm,
        "tpm": run_tpm,
        "burst": run_burst,
        "recovery": run_recovery,
    }
    runner = runners.get(args.mode)
    if runner is None:
        raw_fp.close()
        await caller.close()
        sys.exit(f"unknown mode: {args.mode}")

    try:
        summary = await runner(args, caller, sink)
    finally:
        raw_fp.close()
        await caller.close()

    summary["model"] = args.model
    summary["base_url"] = args.base_url
    summary["timestamp_utc"] = ts
    summary["raw_jsonl"] = str(raw_path.relative_to(RESULTS_DIR.parent))
    sum_path.write_text(json.dumps(summary, indent=2, ensure_ascii=False), encoding="utf-8")
    print(f"\n[done] raw     : {raw_path}")
    print(f"        summary : {sum_path}")

    # Auto-generate markdown report so you don't need to run analyzer manually.
    sys.path.insert(0, str(Path(__file__).parent))
    from analyzer import write_markdown_report  # type: ignore

    write_markdown_report(raw_path, sum_path, md_path)
    print(f"        report  : {md_path}")


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Qwen DashScope rate-limit probe")
    p.add_argument("mode", choices=["concurrency", "rpm", "tpm", "burst", "recovery"])
    p.add_argument("--model", default=DEFAULT_MODEL,
                   help=f"model id (default: {DEFAULT_MODEL})")
    p.add_argument("--base-url", default=DEFAULT_BASE_URL,
                   help="DashScope endpoint")
    p.add_argument("--protocol", default=DEFAULT_PROTOCOL,
                   choices=["anthropic", "openai"],
                   help="API protocol (default: anthropic — DashScope coding endpoint)")
    # concurrency
    p.add_argument("--max-concurrency", type=int, default=64)
    p.add_argument("--cool-down", type=float, default=15.0,
                   help="seconds between concurrency levels (lets RPM/TPM windows reset)")
    # rpm / tpm
    p.add_argument("--target-rpm", type=int, default=120)
    p.add_argument("--duration", type=int, default=90, help="seconds")
    p.add_argument("--tokens-per-request", type=int, default=4000,
                   help="approx prompt size for tpm mode")
    p.add_argument("--completion-tokens", type=int, default=512,
                   help="max output tokens for tpm mode")
    # burst
    p.add_argument("--burst-size", type=int, default=200)
    # recovery
    p.add_argument("--trigger-burst", type=int, default=120)
    p.add_argument("--probe-step", type=float, default=5.0,
                   help="seconds between recovery probes")
    p.add_argument("--max-wait", type=float, default=120.0,
                   help="give up if not recovered after this long")
    return p.parse_args()


if __name__ == "__main__":
    asyncio.run(main_async(parse_args()))
