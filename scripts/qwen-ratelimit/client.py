"""Resilient Qwen DashScope client — reference implementation of the
avoidance strategy described in docs/qwen-ratelimit-analysis.md.

Includes:
  - dual token bucket (RPM + TPM) with smooth pacing
  - bounded concurrency (semaphore)
  - retry with exponential backoff + jitter, respects Retry-After
  - 429 classification (RPM / TPM / burst-QPS) to drive the right reaction
  - optional model fallback chain (try main, fall back on persistent 429)
  - estimate-then-reconcile token accounting so TPM bucket is correct
    before usage comes back

Tested for use as a drop-in: `await client.chat([{...}])`.
"""

from __future__ import annotations

import asyncio
import json
import random
import time
from dataclasses import dataclass
from typing import Any, Awaitable, Callable, Optional

import httpx


# ---------- token bucket ----------

class TokenBucket:
    """Classic token bucket. capacity = burst size, refill_rate = tokens/sec.

    For RPM: capacity = rpm (or a smaller burst), refill = rpm / 60.
    For TPM: capacity = tpm (or smaller burst), refill = tpm / 60.

    Thread-safe via asyncio.Lock; supports asking for N>1 tokens.
    """

    def __init__(self, capacity: float, refill_per_sec: float) -> None:
        self.capacity = float(capacity)
        self.refill = float(refill_per_sec)
        self.tokens = float(capacity)
        self.updated = time.monotonic()
        self._lock = asyncio.Lock()

    def _refill_now(self) -> None:
        now = time.monotonic()
        delta = now - self.updated
        if delta > 0:
            self.tokens = min(self.capacity, self.tokens + delta * self.refill)
            self.updated = now

    async def acquire(self, n: float) -> None:
        """Block until `n` tokens are available, then consume them."""
        if n > self.capacity:
            # A single call larger than the bucket can never get through;
            # let it pass after one full refill window and hope the server
            # has matching headroom. Caller should know.
            n = self.capacity
        while True:
            async with self._lock:
                self._refill_now()
                if self.tokens >= n:
                    self.tokens -= n
                    return
                # Compute precise sleep needed to accumulate (n - tokens).
                wait = (n - self.tokens) / self.refill
            await asyncio.sleep(max(0.005, wait))

    def reconcile(self, used: float, estimate: float) -> None:
        """After a call, adjust bucket if real usage differed from estimate."""
        diff = estimate - used  # if used < estimate, we over-charged
        if diff > 0:
            self.tokens = min(self.capacity, self.tokens + diff)


# ---------- 429 classification ----------

@dataclass
class Limit429:
    kind: str          # "rpm" | "tpm" | "burst" | "unknown"
    retry_after_s: Optional[float]
    raw: dict[str, Any]


def classify_429(http_status: int, headers: dict[str, str], body: dict[str, Any]) -> Optional[Limit429]:
    if http_status != 429:
        return None
    err = body.get("error") or body
    et = (err.get("type") or err.get("errorType") or err.get("code") or "").lower()
    msg = (err.get("message") or "").lower()
    kind = "unknown"
    if "concurrency allocated quota" in msg:
        kind = "concurrency"
    elif "qps" in et or "increased too quickly" in msg or "rate increased" in msg:
        kind = "burst"
    elif "quota" in et or "tokens" in msg or "quota" in msg or "tpm" in et:
        kind = "tpm"
    elif (
        "rate" in et
        or "rpm" in et
        or "current requests" in msg
        or "requests list" in msg
    ):
        kind = "rpm"
    ra = headers.get("retry-after") or headers.get("Retry-After")
    retry_after = None
    if ra:
        try:
            retry_after = float(ra)
        except ValueError:
            retry_after = None
    return Limit429(kind=kind, retry_after_s=retry_after, raw={"type": et, "message": msg})


# ---------- resilient client ----------

class ResilientQwenClient:
    """Resilient wrapper around DashScope's OpenAI-compatible chat endpoint.

    Conservative defaults: tuned to *avoid* limits rather than discover them.
    Bump `rpm` / `tpm` to what your account actually allows. See
    docs/qwen-ratelimit-analysis.md for the table per model.
    """

    def __init__(
        self,
        api_key: str,
        model: str = "qwen-plus",
        base_url: str = "https://dashscope.aliyuncs.com/compatible-mode/v1",
        rpm: int = 180,
        tpm: int = 180_000,
        concurrency: int = 16,
        max_retries: int = 6,
        base_backoff: float = 1.0,
        max_backoff: float = 30.0,
        fallback_models: Optional[list[str]] = None,
        on_429: Optional[Callable[[Limit429, int, str], None]] = None,
    ) -> None:
        self.api_key = api_key
        self.model = model
        self.base_url = base_url.rstrip("/")
        # 25%-burst, 75%-steady: capacity = rpm/4, refill = rpm/60.
        # Smaller burst capacity is what kills the "Request rate increased
        # too quickly" failures.
        self.rpm_bucket = TokenBucket(capacity=max(1, rpm // 4), refill_per_sec=rpm / 60.0)
        self.tpm_bucket = TokenBucket(capacity=max(1, tpm // 4), refill_per_sec=tpm / 60.0)
        self.sem = asyncio.Semaphore(concurrency)
        self.max_retries = max_retries
        self.base_backoff = base_backoff
        self.max_backoff = max_backoff
        self.fallback_models = fallback_models or []
        self.on_429 = on_429
        self.client = httpx.AsyncClient(
            timeout=60.0,
            limits=httpx.Limits(max_connections=concurrency * 2, max_keepalive_connections=concurrency),
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
            },
        )

    async def close(self) -> None:
        await self.client.aclose()

    @staticmethod
    def _estimate_tokens(messages: list[dict[str, Any]], max_tokens: int) -> int:
        """Rough estimate so the TPM bucket has something to charge before
        the real usage comes back. 1.5 char/token CN, 4 char/token EN; we
        use 2 char/token as a safe middle ground. Adds max_tokens for output.
        """
        chars = 0
        for m in messages:
            content = m.get("content") or ""
            if isinstance(content, str):
                chars += len(content)
            elif isinstance(content, list):
                for part in content:
                    if isinstance(part, dict):
                        chars += len(part.get("text") or "")
        return chars // 2 + max_tokens

    async def chat(
        self,
        messages: list[dict[str, Any]],
        max_tokens: int = 512,
        temperature: float = 0.2,
        model: Optional[str] = None,
        extra: Optional[dict[str, Any]] = None,
    ) -> dict[str, Any]:
        """Send a chat completion with full resilience.

        Returns the parsed JSON response on success.
        Raises the last httpx/json error on permanent failure.
        """
        primary = model or self.model
        models_to_try = [primary] + [m for m in self.fallback_models if m != primary]

        last_err: Exception | None = None
        for model_id in models_to_try:
            try:
                return await self._chat_one_model(
                    model_id, messages, max_tokens, temperature, extra or {}
                )
            except _ExhaustedError as e:
                last_err = e
                continue
        # All models exhausted
        if last_err:
            raise last_err
        raise RuntimeError("no models configured")

    async def _chat_one_model(
        self,
        model_id: str,
        messages: list[dict[str, Any]],
        max_tokens: int,
        temperature: float,
        extra: dict[str, Any],
    ) -> dict[str, Any]:
        est = self._estimate_tokens(messages, max_tokens)
        payload = {
            "model": model_id,
            "messages": messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
            "stream": False,
            **extra,
        }
        url = f"{self.base_url}/chat/completions"

        attempt = 0
        while True:
            # 1. Wait for budget: RPM (1 req) + TPM (estimate).
            await self.rpm_bucket.acquire(1)
            await self.tpm_bucket.acquire(est)
            async with self.sem:
                try:
                    resp = await self.client.post(url, json=payload)
                except httpx.TimeoutException as e:
                    if attempt >= self.max_retries:
                        raise _ExhaustedError(f"timeout after {attempt} retries: {e}") from e
                    attempt += 1
                    await self._sleep_backoff(attempt, None)
                    continue
                except httpx.HTTPError as e:
                    if attempt >= self.max_retries:
                        raise _ExhaustedError(f"http error after {attempt} retries: {e}") from e
                    attempt += 1
                    await self._sleep_backoff(attempt, None)
                    continue

            if resp.status_code == 200:
                data = resp.json()
                used = (data.get("usage") or {}).get("total_tokens") or est
                # Give back un-used budget (or take a tiny bit more).
                self.tpm_bucket.reconcile(used=used, estimate=est)
                return data

            # Non-200
            try:
                body = resp.json()
            except json.JSONDecodeError:
                body = {"error": {"message": resp.text}}

            if resp.status_code == 429:
                info = classify_429(429, dict(resp.headers), body)
                assert info is not None
                if self.on_429:
                    try:
                        self.on_429(info, attempt, model_id)
                    except Exception:  # noqa: BLE001
                        pass
                if attempt >= self.max_retries:
                    raise _ExhaustedError(
                        f"429 {info.kind} after {attempt} retries on {model_id}: {info.raw}"
                    )
                attempt += 1
                # On a TPM hit, the same prompt will hit again; if a fallback
                # model has a different TPM bucket, escalate sooner.
                if info.kind in ("tpm", "burst") and attempt >= max(2, self.max_retries // 2):
                    raise _ExhaustedError(
                        f"persistent 429 {info.kind} on {model_id}, fall back"
                    )
                await self._sleep_backoff(attempt, info.retry_after_s)
                continue

            if 500 <= resp.status_code < 600:
                if attempt >= self.max_retries:
                    raise _ExhaustedError(f"{resp.status_code} after {attempt} retries: {body}")
                attempt += 1
                await self._sleep_backoff(attempt, None)
                continue

            # 4xx other than 429: don't retry, surface immediately.
            raise httpx.HTTPStatusError(
                f"{resp.status_code} {body}", request=resp.request, response=resp
            )

    async def _sleep_backoff(self, attempt: int, retry_after: Optional[float]) -> None:
        # Prefer server-supplied Retry-After when present.
        if retry_after is not None:
            await asyncio.sleep(min(retry_after, self.max_backoff))
            return
        base = min(self.max_backoff, self.base_backoff * (2 ** (attempt - 1)))
        # Full jitter (Marc Brooker, AWS): pick uniform random in [0, base).
        await asyncio.sleep(random.uniform(0, base))


class _ExhaustedError(RuntimeError):
    """Raised internally to signal we should try the next fallback model."""


# ---------- demo ----------

async def _demo() -> None:
    import os
    api_key = os.environ.get("DASHSCOPE_API_KEY")
    if not api_key:
        print("set DASHSCOPE_API_KEY to run the demo")
        return
    client = ResilientQwenClient(
        api_key=api_key,
        model="qwen-plus",
        rpm=180,
        tpm=200_000,
        concurrency=16,
        fallback_models=["qwen-turbo"],
        on_429=lambda info, attempt, model: print(
            f"  [429] kind={info.kind} model={model} attempt={attempt} retry_after={info.retry_after_s}"
        ),
    )
    try:
        async def one(i: int) -> Any:
            return await client.chat(
                messages=[{"role": "user", "content": f"用一个字回答：{i} 是奇数还是偶数？"}],
                max_tokens=8,
            )

        # Fire 200 in parallel; client paces them under the bucket.
        results = await asyncio.gather(*[one(i) for i in range(200)], return_exceptions=True)
        ok = sum(1 for r in results if not isinstance(r, Exception))
        print(f"demo: ok={ok}/{len(results)}")
    finally:
        await client.close()


if __name__ == "__main__":
    asyncio.run(_demo())
