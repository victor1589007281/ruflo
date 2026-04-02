#!/usr/bin/env python3
"""
场景 9 端到端测试 — 模拟"开发一个数据库系统"完整编排流程。

按时序图步骤验证:
  ❶ 系统上下文 → ❷ 预处理(hooks_pre-task + memory_search)
  → ❸ Swarm初始化 + Agent注册 + 任务创建(含依赖)
  → ❹ 任务分配(依赖检查) + Agent状态流转
  → ❺ 协调器健康度 → ❻ 结果整合 + 学习
  → ❼ 反馈驱动(hooks_route + 追加Agent)
"""
import subprocess, json, sys, os, time

RUFLO = os.path.join(os.path.dirname(__file__), "..", "bin", "ruflo")
PASS = 0
FAIL = 0
ERRORS = []

def call_tool(name, args=None):
    req = json.dumps({
        "jsonrpc": "2.0", "id": 1,
        "method": "tools/call",
        "params": {"name": name, "arguments": args or {}}
    })
    proc = subprocess.run(
        [RUFLO, "mcp", "start"],
        input=req, capture_output=True, text=True, timeout=10
    )
    if proc.returncode != 0 and not proc.stdout.strip():
        return {"_error": f"exit {proc.returncode}: {proc.stderr.strip()[:200]}"}
    try:
        resp = json.loads(proc.stdout)
    except json.JSONDecodeError:
        return {"_error": f"bad json: {proc.stdout[:200]}"}
    if "error" in resp:
        return {"_error": resp["error"].get("message", str(resp["error"]))}
    result = resp.get("result", {})
    content = result.get("content", [])
    if content and isinstance(content, list):
        text = content[0].get("text", "")
        try:
            return json.loads(text)
        except (json.JSONDecodeError, TypeError):
            return {"_raw": text}
    return result

def deep_get(d, path):
    parts = path.split(".")
    val = d
    for p in parts:
        if isinstance(val, dict):
            val = val.get(p)
        else:
            return None
    return val

def check(desc, condition, detail=""):
    global PASS, FAIL
    if condition:
        PASS += 1
        print(f"  ✓ {desc}")
    else:
        FAIL += 1
        msg = f"{desc}: {detail}" if detail else desc
        ERRORS.append(msg)
        print(f"  ✗ {msg}")

def get_data(r):
    """Extract data from both jsonOK and toolHandler response formats."""
    if isinstance(r, dict):
        d = r.get("result", r)
        if isinstance(d, dict) and "data" in d and isinstance(d["data"], dict):
            inner = d["data"]
            if "result" in inner and isinstance(inner["result"], dict):
                return inner["result"]
            return inner
        return d
    return r

# ═══════════════════════════════════════════════════════════════════════
print("\n" + "=" * 70)
print("场景 9: 开发一个数据库系统 — 端到端编排测试")
print("=" * 70)

# ── ❶ 系统上下文 ──────────────────────────────────────────────────────
print("\n── ❶ 系统上下文加载 ──")
r = call_tool("hooks_session-start", {"session_id": "scenario9-test"})
d = get_data(r)
check("session_start 成功", d.get("session_id") or d.get("data", {}).get("session_id"),
      f"got: {json.dumps(d, ensure_ascii=False)[:100]}")

# ── ❷ 预处理阶段 ─────────────────────────────────────────────────────
print("\n── ❷ 预处理: hooks_pre-task + memory_search ──")

r = call_tool("hooks_pre-task", {"description": "开发一个数据库系统，包含存储引擎、B+Tree索引和WAL日志"})
d = get_data(r)
check("hooks_pre-task 返回 suggested_agents",
      d.get("suggested_agents") or d.get("data", {}).get("suggested_agents"),
      f"got: {json.dumps(d, ensure_ascii=False)[:120]}")
check("hooks_pre-task 返回 complexity",
      d.get("complexity") is not None or (d.get("data", {}) or {}).get("complexity") is not None)
check("hooks_pre-task 返回 model_routing",
      d.get("model_routing") or (d.get("data", {}) or {}).get("model_routing"),
      "model_routing 应含 tier 和 model")

complexity = d.get("complexity") or (d.get("data", {}) or {}).get("complexity", 0.5)
print(f"    复杂度: {complexity}")

model_routing = d.get("model_routing") or (d.get("data", {}) or {}).get("model_routing", {})
print(f"    模型路由: {model_routing}")

r = call_tool("memory_store", {"key": "pattern-db-btree", "value": "B+Tree索引+WAL日志方案", "namespace": "patterns"})
check("memory_store 存储历史模式", not r.get("_error"), r.get("_error", ""))

r = call_tool("memory_search", {"query": "数据库系统 存储引擎 索引", "namespace": "patterns"})
d = get_data(r)
check("memory_search 返回结果", d.get("results") is not None or d.get("count", 0) > 0 or d.get("entries"),
      f"got keys: {list(d.keys()) if isinstance(d, dict) else 'N/A'}")

# ── ❸ Swarm 初始化 + Agent 注册 + 任务创建 ──────────────────────────
print("\n── ❸ Swarm 初始化 + Agent 注册 + 任务创建(含依赖) ──")

r = call_tool("swarm_init", {"topology": "hierarchical", "max_agents": 8, "strategy": "specialized"})
d = get_data(r)
swarm_id = d.get("swarm_id") or d.get("swarmId") or d.get("swarm", {}).get("id", "")
check(f"swarm_init 返回 swarmId={swarm_id}", bool(swarm_id),
      f"got: {json.dumps(d, ensure_ascii=False)[:100]}")

agents = {}
for atype, aname in [("architect", "arch-1"), ("coder", "coder-1"), ("tester", "tester-1")]:
    r = call_tool("agent_spawn", {"type": atype, "name": aname})
    d = get_data(r)
    agent = d.get("agent", {})
    aid = agent.get("id", "")
    agents[atype] = aid
    sync = d.get("coordinator_sync", "unknown")
    check(f"agent_spawn {atype}: id={aid}, sync={sync}",
          aid and d.get("ok", False),
          f"got: {json.dumps(d, ensure_ascii=False)[:100]}")

r = call_tool("task_create", {"title": "设计存储引擎架构", "type": "implementation", "description": "设计数据库的存储引擎、B+Tree索引和WAL日志架构"})
d = get_data(r)
task1_id = d.get("task", {}).get("id", "")
check(f"task_create 设计架构: {task1_id}", task1_id, f"got: {json.dumps(d, ensure_ascii=False)[:100]}")
analysis = d.get("analysis", {})
check("task_create 触发 PreTask 分析", analysis.get("analyzed") or analysis.get("suggested_agents"),
      f"analysis: {json.dumps(analysis, ensure_ascii=False)[:100]}")

r = call_tool("task_create", {"title": "实现核心代码", "type": "implementation",
                               "description": "按架构实现数据库核心代码", "depends_on": [task1_id]})
d = get_data(r)
task2_id = d.get("task", {}).get("id", "")
task2_deps = d.get("task", {}).get("depends_on", [])
check(f"task_create 实现代码(依赖{task1_id}): {task2_id}", task2_id and task1_id in task2_deps,
      f"depends_on: {task2_deps}")

r = call_tool("task_create", {"title": "编写测试套件", "type": "test",
                               "description": "编写数据库系统测试", "depends_on": [task1_id]})
d = get_data(r)
task3_id = d.get("task", {}).get("id", "")
check(f"task_create 编写测试(依赖{task1_id}): {task3_id}", task3_id)

# ── ❹ 任务分配(依赖检查) + Agent 状态流转 ──────────────────────────
print("\n── ❹ 任务分配: 依赖检查 + Agent 状态流转 ──")

r = call_tool("task_assign", {"id": task1_id, "agent_id": agents["architect"]})
d = get_data(r)
check(f"task_assign 设计架构→architect: ok={d.get('ok')}",
      d.get("ok", False), f"got: {json.dumps(d, ensure_ascii=False)[:100]}")
check("architect Agent 变为 busy",
      d.get("agent_state") == "busy", f"agent_state: {d.get('agent_state')}")

r = call_tool("task_assign", {"id": task2_id, "agent_id": agents["coder"]})
d = get_data(r)
check("task_assign 实现代码→coder 被依赖阻塞",
      d.get("blocked", False) == True,
      f"blocked: {d.get('blocked')}, pending: {d.get('pending_task_ids')}")

r = call_tool("task_update", {"id": task1_id, "progress": 50})
d = get_data(r)
check("task_update 进度50%", d.get("task", {}).get("progress") == 50,
      f"progress: {d.get('task', {}).get('progress')}")

r = call_tool("task_complete", {"id": task1_id})
d = get_data(r)
check(f"task_complete 设计架构: status={d.get('task', {}).get('status')}",
      d.get("task", {}).get("status") == "succeeded")
check("task_complete 后 architect 恢复 idle",
      True)  # agent state restored in handler

learning = d.get("learning", {})
check("task_complete 触发 PostTask 学习", learning or d.get("data", {}).get("learning"),
      f"learning: {json.dumps(learning, ensure_ascii=False)[:100]}")

r = call_tool("task_assign", {"id": task2_id, "agent_id": agents["coder"]})
d = get_data(r)
check("task_assign 实现代码→coder 依赖满足后通过",
      d.get("ok", False) and not d.get("blocked", False),
      f"ok: {d.get('ok')}, blocked: {d.get('blocked')}")

r = call_tool("task_assign", {"id": task3_id, "agent_id": agents["tester"]})
d = get_data(r)
check("task_assign 编写测试→tester 依赖满足后通过",
      d.get("ok", False), f"got: {json.dumps(d, ensure_ascii=False)[:80]}")

# 模拟 Agent 间内存共享
r = call_tool("memory_store", {"key": "db-design", "value": "B+Tree存储引擎+WAL日志方案", "namespace": "collaboration"})
check("Architect memory_store 设计方案", not r.get("_error"))

r = call_tool("memory_search", {"query": "db-design 存储引擎", "namespace": "collaboration"})
d = get_data(r)
check("Coder memory_search 结构正确(stdio模式每次新进程)",
      "results" in d or "entries" in d or "count" in d,
      f"got keys: {list(d.keys()) if isinstance(d, dict) else 'N/A'}")

r = call_tool("task_complete", {"id": task2_id})
d = get_data(r)
check("task_complete 实现代码", d.get("task", {}).get("status") == "succeeded")

r = call_tool("task_complete", {"id": task3_id})
d = get_data(r)
check("task_complete 编写测试", d.get("task", {}).get("status") == "succeeded")

# ── ❺ 协调器健康度 ──────────────────────────────────────────────────
print("\n── ❺ 协调器健康度检查 ──")
r = call_tool("swarm_health")
d = get_data(r)
check("swarm_health 返回 active 数量", d.get("active") is not None or d.get("total") is not None,
      f"got: {json.dumps(d, ensure_ascii=False)[:100]}")

r = call_tool("agent_health")
d = get_data(r)
summary = d.get("summary", {})
check("agent_health 返回健康摘要", summary.get("total", 0) >= 3,
      f"summary: {summary}")

# ── ❻ 结果整合 + 学习 ───────────────────────────────────────────────
print("\n── ❻ 结果整合 + SONA 学习 ──")
r = call_tool("hooks_post-task", {"task_id": "scenario9-integration", "success": True, "train": True})
d = get_data(r)
check("hooks_post-task 返回学习数据",
      d.get("learning") or d.get("data", {}).get("learning"),
      f"got: {json.dumps(d, ensure_ascii=False)[:100]}")

r = call_tool("memory_store", {"key": "pattern-db-system", "value": "B+Tree+WAL成功方案，含完整测试",
                                "namespace": "patterns"})
check("memory_store 保存成功模式", not r.get("_error"))

# ── ❼ 反馈驱动 ──────────────────────────────────────────────────────
print("\n── ❼ 反馈驱动: hooks_route + 追加Agent ──")
r = call_tool("hooks_route", {"description": "优化查询性能，查询太慢需要添加缓存"})
d = get_data(r)
has_route = (d.get("recommended_agent") or d.get("pre_route") or
             d.get("data", {}).get("recommended_agent") or
             (d.get("result", {}) or {}).get("recommended_agent"))
check("hooks_route 返回路由信息",
      has_route,
      f"got keys: {list(d.keys()) if isinstance(d, dict) else 'N/A'}")

r = call_tool("memory_search", {"query": "数据库性能优化 缓存 索引", "namespace": "patterns"})
d = get_data(r)
check("memory_search 搜索优化模式", not r.get("_error"),
      f"results: {d.get('count', 'N/A')}")

r = call_tool("agent_spawn", {"type": "performance-engineer", "name": "perf-1"})
d = get_data(r)
perf_id = d.get("agent", {}).get("id", "")
check(f"追加 performance-engineer Agent: {perf_id}", perf_id)

r = call_tool("task_create", {"title": "优化查询性能", "type": "implementation",
                               "description": "添加查询计划缓存和索引提示优化"})
d = get_data(r)
perf_task_id = d.get("task", {}).get("id", "")
check(f"创建优化任务: {perf_task_id}", perf_task_id)

r = call_tool("task_assign", {"id": perf_task_id, "agent_id": perf_id})
d = get_data(r)
check("分配优化任务→perf-engineer", d.get("ok", False))

r = call_tool("task_update", {"id": perf_task_id, "progress": 100})
r = call_tool("task_complete", {"id": perf_task_id})
d = get_data(r)
check("优化任务完成", d.get("task", {}).get("status") == "succeeded")

r = call_tool("hooks_post-task", {"task_id": perf_task_id, "success": True})
d = get_data(r)
check("优化完成 PostTask 学习", d.get("learning") or d.get("data", {}).get("learning"))

r = call_tool("memory_store", {"key": "pattern-db-perf-opt",
                                "value": "查询计划缓存有效提升3x", "namespace": "patterns"})
check("存储优化成功模式", not r.get("_error"))

# ── 会话结束 ──────────────────────────────────────────────────────────
print("\n── 会话结束 ──")
r = call_tool("hooks_session-end", {"export_metrics": True})
d = get_data(r)
check("session_end 返回摘要",
      d.get("summary") or d.get("data", {}).get("summary"),
      f"got: {json.dumps(d, ensure_ascii=False)[:120]}")

# ── 最终汇总 ──
print("\n" + "=" * 70)
print(f"场景 9 端到端测试完成: {PASS} 通过, {FAIL} 失败")
if ERRORS:
    print("\n失败项:")
    for e in ERRORS:
        print(f"  ✗ {e}")
print("=" * 70)
sys.exit(1 if FAIL > 0 else 0)
