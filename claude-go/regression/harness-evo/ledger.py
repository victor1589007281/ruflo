#!/usr/bin/env python3
# ledger.py —— harness 进化效果台账 (手册 7.5)。
#
# 唯一数据真相源: regression/harness-evo/ledger.jsonl (append-only, 随仓提交)。
# 每次关键功能点改造后跑电池 → 追加台账 → 重新生成 docforge 7.5 的图表数据块。
#
# 用法:
#   python3 ledger.py append-k8s    <电池结果.jsonl> [--change "说明"] [--ts ISO] [--commit SHA]
#   python3 ledger.py append-replay <replay --out.jsonl> --model M [--mode baseline|candidate] [--change "说明"]
#   python3 ledger.py report                     # 文本汇总 (每模型×档位 最新通过率/耗时)
#   python3 ledger.py xychart                    # 生成 mermaid xychart 数据块 (直接贴进 7.5 页面)
#
# 行格式 (v1):
#   {"v":1,"ts":"...","commit":"...","change":"...","track":"k8s-ab|replay",
#    "suite":"engine-basic-v1|coding-basic-v1","model":"lfm2.5:2.6b-q4_k_m",
#    "mode":"baseline|plan3|candidate","task":"t1","pass":1,"wall_s":26,
#    "valid":1,"note":"","source":"/tmp/wml-e2e-results-....jsonl"}
# valid=0: 该轮因基础设施/评测缺陷无效 (如 ConfigMap 覆盖致全 rc=1), 入台账留痕但不进趋势图。
import argparse
import json
import os
import re
import subprocess
import sys
from collections import defaultdict
from datetime import datetime, timezone, timedelta

HERE = os.path.dirname(os.path.abspath(__file__))
LEDGER = os.environ.get("HARNESS_EVO_LEDGER", os.path.join(HERE, "ledger.jsonl"))
CST = timezone(timedelta(hours=8))


def now_iso():
    return datetime.now(CST).isoformat(timespec="seconds")


def git_commit():
    try:
        return subprocess.run(["git", "rev-parse", "--short", "HEAD"], cwd=HERE,
                              capture_output=True, text=True, timeout=10).stdout.strip()
    except Exception:
        return ""


def norm_model(m):
    return m[len("ollama:"):] if m.startswith("ollama:") else m


def load_ledger(path=LEDGER):
    rows = []
    if not os.path.exists(path):
        return rows
    with open(path, encoding="utf-8") as f:
        for ln, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            try:
                r = json.loads(line)
            except json.JSONDecodeError:
                sys.exit(f"台账第 {ln} 行 JSON 损坏, 先人工修复: {line[:80]}")
            if r.get("v") != 1:
                sys.exit(f"台账第 {ln} 行版本未知 (v={r.get('v')}), 先升级 ledger.py")
            rows.append(r)
    return rows


def append_rows(rows, path=LEDGER):
    # 幂等: 同 (track,suite,model,mode,task,source) 已存在则跳过
    existing = {(r["track"], r["suite"], r["model"], r["mode"], r["task"], r.get("source", ""))
                for r in load_ledger(path)}
    n_new = 0
    with open(path, "a", encoding="utf-8") as f:
        for r in rows:
            key = (r["track"], r["suite"], r["model"], r["mode"], r["task"], r.get("source", ""))
            if key in existing:
                continue
            f.write(json.dumps(r, ensure_ascii=False) + "\n")
            n_new += 1
    print(f"台账追加 {n_new} 行 (跳过重复 {len(rows) - n_new} 行) → {path}")


# ━━━ 轨 A: 引擎级 k8s A/B 电池 ━━━

# 早期结果文件有 "grep -c || echo 0" 双输出缺陷 (计数段断行, JSON 非法),
# 核心字段 (model/task/wml/rc/pass/wall_s) 在断行之前, 用正则兜底解析。
K8S_CORE = re.compile(
    r'\{"model":"(?P<model>[^"]+)","task":"(?P<task>[^"]+)","wml":(?P<wml>[01]),'
    r'"rc":(?P<rc>-?\d+),"pass":(?P<pass>[01]),"wall_s":(?P<wall>\d+)')


def parse_k8s_results(path):
    rows = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            m = K8S_CORE.search(line)
            if not m:
                continue
            d = m.groupdict()
            rows.append({
                "model": norm_model(d["model"]), "task": d["task"],
                "mode": "plan3" if d["wml"] == "1" else "baseline",
                "rc": int(d["rc"]), "pass": int(d["pass"]), "wall_s": int(d["wall"]),
            })
    return rows


def cmd_append_k8s(a):
    raw = parse_k8s_results(a.results_file)
    if not raw:
        sys.exit(f"结果文件无有效行: {a.results_file}")
    # 全部 rc!=0 → 整轮无效 (基础设施失败), valid=0 留痕
    valid = 0 if all(r["rc"] != 0 for r in raw) else 1
    note = a.note or ""
    if valid == 0 and "基础设施" not in note:
        note = (note + " " if note else "") + "全部 rc≠0, 判基础设施失败 (valid=0)"
    ts = a.ts or now_iso()
    rows = [{
        "v": 1, "ts": ts, "commit": a.commit if a.commit is not None else git_commit(),
        "change": a.change or "", "track": "k8s-ab", "suite": "engine-basic-v1",
        "model": r["model"], "mode": r["mode"], "task": r["task"],
        "pass": r["pass"], "wall_s": r["wall_s"], "valid": valid,
        "note": note, "source": a.results_file,
    } for r in raw]
    append_rows(rows, a.ledger)


# ━━━ 轨 B: replay 单发评测 (evo replay --out, 行格式 = replay.Result) ━━━

def cmd_append_replay(a):
    rows = []
    with open(a.replay_out, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            r = json.loads(line)  # {"fingerprint","task_id","score","passed","reason"}
            rows.append({
                "v": 1, "ts": a.ts or now_iso(),
                "commit": a.commit if a.commit is not None else git_commit(),
                "change": a.change or "", "track": "replay", "suite": a.suite,
                "model": norm_model(a.model), "mode": a.mode, "task": r["task_id"],
                "pass": 1 if r.get("passed") else 0, "score": r.get("score", 0),
                "wall_s": 0, "valid": 1, "note": a.note or "", "source": a.replay_out,
            })
    if not rows:
        sys.exit(f"replay 结果为空: {a.replay_out}")
    append_rows(rows, a.ledger)


# ━━━ 汇总与图表数据 ━━━

def group_key(r):
    return (r["track"], r["model"], r["mode"])


def cmd_report(a):
    rows = [r for r in load_ledger(a.ledger)]
    if not rows:
        print("台账为空")
        return
    agg = defaultdict(lambda: {"n": 0, "pass": 0, "wall": 0, "valid_n": 0, "last": ""})
    for r in rows:
        k = group_key(r)
        g = agg[k]
        g["n"] += 1
        if r.get("valid", 1):
            g["valid_n"] += 1
            g["pass"] += r["pass"]
            g["wall"] += r.get("wall_s", 0)
            g["last"] = max(g["last"], r["ts"])
    print(f"{'track':<8}{'model':<26}{'mode':<10}{'rows':>5}{'valid':>6}{'pass%':>7}{'avg_wall_s':>11}  last")
    for (track, model, mode), g in sorted(agg.items()):
        if g["valid_n"]:
            print(f"{track:<8}{model:<26}{mode:<10}{g['n']:>5}{g['valid_n']:>6}"
                  f"{100 * g['pass'] / g['valid_n']:>6.0f}%{g['wall'] / g['valid_n']:>10.0f}  {g['last']}")
        else:
            print(f"{track:<8}{model:<26}{mode:<10}{g['n']:>5}{0:>6}{'--':>7}{'--':>11}  {g['last']} (全无效)")


def run_points(rows, track, model):
    """有效行按 (日期, 当日第几轮) 聚合 → [(label, {mode: pass%}, ts)] 时序点。"""
    valid = [r for r in rows if r["track"] == track and r["model"] == model and r.get("valid", 1)]
    by_run = defaultdict(list)
    for r in valid:
        by_run[r["ts"][:13]].append(r)  # 同一小时视为同一轮
    points = []
    for i, (hour, rs) in enumerate(sorted(by_run.items())):
        label = f"{hour[5:7]}-{hour[8:10]}"  # MM-DD
        same_day = sorted(h for h in by_run if h[:10] == hour[:10])
        if len(same_day) > 1:
            label += chr(ord('a') + same_day.index(hour))  # 同日多轮: 12-08a / 12-08b
        per_mode = {}
        for mode in ("baseline", "plan3", "candidate"):
            mr = [r for r in rs if r["mode"] == mode]
            if mr:
                per_mode[mode] = round(100 * sum(r["pass"] for r in mr) / len(mr), 1)
        points.append((label, per_mode, hour))
    return points


def cmd_xychart(a):
    rows = load_ledger(a.ledger)
    models = sorted({r["model"] for r in rows if r["track"] == "k8s-ab"})
    for model in models:
        pts = run_points(rows, "k8s-ab", model)
        if not pts:
            continue
        modes = sorted({m for _, pm, _ in pts for m in pm},
                       key=lambda m: ("baseline", "plan3", "candidate").index(m))
        print(f"\n<!-- {model} 引擎级通过率 (轨 A, 有效轮次) -->")
        print('xychart-beta')
        print(f'    title "{model} 引擎级电池通过率趋势 (%)"')
        print("    x-axis [" + ",".join(f'"{p[0]}"' for p in pts) + "]")
        print('    y-axis "pass %" 0 --> 100')
        for mode in modes:
            vals = [str(pm[mode]) if mode in pm else "0" for _, pm, _ in pts]
            print(f"    line [{','.join(vals)}]")
        print(f"%% 系列顺序: {' → '.join(modes)} (更新时保持此顺序, 颜色跟随身份)")


def main():
    ap = argparse.ArgumentParser(description="harness 进化台账 (手册 7.5)")
    ap.add_argument("--ledger", default=LEDGER)
    sub = ap.add_subparsers(dest="cmd", required=True)

    for name in ("append-k8s", "append-replay"):
        p = sub.add_parser(name)
        p.add_argument("results_file" if name == "append-k8s" else "replay_out")
        p.add_argument("--change", default="")
        p.add_argument("--note", default="")
        p.add_argument("--ts", default=None)
        p.add_argument("--commit", default=None)
        p.add_argument("--ledger", default=LEDGER)
        if name == "append-replay":
            p.add_argument("--model", required=True)
            p.add_argument("--mode", default="baseline",
                           choices=["baseline", "plan3", "candidate"])
            p.add_argument("--suite", default="coding-basic-v1")
        p.set_defaults(fn=cmd_append_k8s if name == "append-k8s" else cmd_append_replay)

    for name, fn in (("report", cmd_report), ("xychart", cmd_xychart)):
        p = sub.add_parser(name)
        p.add_argument("--ledger", default=LEDGER)
        p.set_defaults(fn=fn)

    a = ap.parse_args()
    a.fn(a)


if __name__ == "__main__":
    main()
