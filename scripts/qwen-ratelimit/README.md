# Qwen DashScope rate-limit probe

Five-mode probe to figure out where DashScope starts rate-limiting *your*
account, plus a reference resilient client (`client.py`).

Supports **both** OpenAI-compatible (`compatible-mode/v1`) and
**Anthropic-compatible** (`coding.dashscope.aliyuncs.com/apps/anthropic/v1`)
endpoints — the latter is what `claude-go` uses by default with model
`qwen3.6-plus`.

Full analysis and recommended config: [`docs/qwen-ratelimit-analysis.md`](../../docs/qwen-ratelimit-analysis.md).

## Files

| file              | purpose |
|-------------------|---------|
| `probe.py`        | the probe driver, 5 modes, dual protocol |
| `analyzer.py`     | turns raw `.jsonl` + `.json` into a Markdown report (auto-invoked by `probe.py`) |
| `client.py`       | drop-in resilient client (token bucket / concurrency limit + retry + fallback) |
| `requirements.txt`| just `httpx` |
| `.env.example`    | env var template |
| `results/`        | output: `<model>_<mode>_<ts>.{jsonl,json,md}` |

## Setup

```bash
cd scripts/qwen-ratelimit
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env       # then edit .env
export $(grep -v '^#' .env | xargs)   # or: source .env
```

Get a key from the Alibaba Cloud Model Studio console (百炼).

## Five probe modes

### 1. `concurrency` — find the burst/QPS ceiling

Step concurrency 1, 2, 4, 8, 16, 32, … and look for the first level where
429s appear. Cools down for 15s between levels so RPM/TPM windows reset.

```bash
# OpenAI-compatible endpoint
python probe.py concurrency --model qwen-plus --max-concurrency 128 --cool-down 15

# Anthropic-compatible coding endpoint (claude-go default)
python probe.py concurrency --model qwen3.6-plus --protocol anthropic --max-concurrency 16
```

### 2. `rpm` — find the RPM ceiling

Hold a steady RPM for `duration` seconds, see when failures start.

```bash
python probe.py rpm --target-rpm 60 --duration 90
```

### 3. `tpm` — find the TPM ceiling

Send big prompts at moderate RPM; saturates tokens before request count.

```bash
python probe.py tpm --tokens-per-request 4000 \
  --completion-tokens 512 --target-rpm 60 --duration 120
```

### 4. `burst` — single-shot burst guard test

Fire K requests in one shot, observe what fraction the burst guard kills.

```bash
python probe.py burst --burst-size 50
```

### 5. `recovery` — measure 429 recovery time

Trigger 429 with a burst, then probe every N seconds until you get a 200.

```bash
python probe.py recovery --trigger-burst 50 --probe-step 2 --max-wait 30
```

## Output

Each run produces three files under `results/<model>_<mode>_<ts>.*`:

- `*.jsonl` — one record per request (latency, status, error_type,
  tokens, rate-limit headers)
- `*.json`  — mode-level summary
- `*.md`    — human-readable report with rolling windows, error
  breakdown, error samples, and inferred ceilings

## Recommended exploration order

```bash
# 1. Find the steady-state ceiling.
python probe.py rpm --target-rpm 30  --duration 60
python probe.py rpm --target-rpm 60  --duration 60
python probe.py rpm --target-rpm 120 --duration 60

# 2. Find the burst ceiling.
python probe.py burst --burst-size 10
python probe.py burst --burst-size 50

# 3. Find TPM (if applicable to your endpoint).
python probe.py tpm --target-rpm 3 --tokens-per-request 8000 --duration 60

# 4. Confirm recovery time.
python probe.py recovery --trigger-burst 50 --probe-step 2 --max-wait 30
```

Each individual run is small (60–120s), but probing aggressively *will*
spend money and hit rate limits.

## Using `client.py` as a drop-in

### Standard endpoint (RPM/TPM limited)

```python
from client import ResilientQwenClient

c = ResilientQwenClient(
    api_key=os.environ["DASHSCOPE_API_KEY"],
    model="qwen-plus",
    rpm=180,
    tpm=180_000,
    concurrency=16,
    fallback_models=["qwen-turbo"],
)
resp = await c.chat([{"role": "user", "content": "hi"}])
```

### Coding endpoint (concurrency limited, ~7–9 slots)

```python
c = ResilientQwenClient(
    api_key=os.environ["DASHSCOPE_API_KEY"],
    base_url="https://coding.dashscope.aliyuncs.com/apps/anthropic/v1",
    model="qwen3.6-plus",
    rpm=0,                # not sensitive on this endpoint
    tpm=0,
    concurrency=6,        # hard ceiling — the most important param
    max_retries=3,
    base_backoff=2.0,     # ~2s实测恢复时间
    max_backoff=5.0,
)
resp = await c.chat([{"role": "user", "content": "hi"}])
```
