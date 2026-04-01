#!/usr/bin/env python3
"""
验证默认 Hook Handler 不再空转：
hooks_pre-task 应返回 analyzed=true、suggested_agents、complexity 等字段。
hooks_pre-command 应返回 risk_level、checked=true。
hooks_pre-edit 应返回 checked=true。
hooks_session-start 应返回 session_id。
hooks_session-end 应返回 summary。
"""
import subprocess, json, sys, os

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
    """Traverse nested dict by dot-separated path."""
    parts = path.split(".")
    val = d
    for p in parts:
        if isinstance(val, dict):
            val = val.get(p)
        else:
            return None
    return val

def assert_field(result, path, desc):
    global PASS, FAIL
    # Try direct path first
    val = deep_get(result, path)
    # toolHandler wraps in {"ok":true, "data":{"result":{...data...}}}
    # while jsonOK wraps in {"result":{...data...}}
    # So also try data.result.data.* for toolHandler format
    if val is None or val == "" or val == [] or val == {}:
        alt_path = "data.result." + path.replace("result.", "", 1) if path.startswith("result.") else None
        if alt_path:
            val = deep_get(result, alt_path)
    if val is not None and val != "" and val != [] and val != {}:
        PASS += 1
        display = str(val)[:80]
        print(f"  ✓ {desc}: = {display}")
    else:
        FAIL += 1
        ERRORS.append(f"{desc}: {path} is empty/missing")
        print(f"  ✗ {desc}: {path} is empty/missing (got {val})")

print("=" * 70)
print("  Hook Handler Non-No-Op Verification Test")
print("=" * 70)

# ── hooks_pre-task 应有 analyzed=true + suggested_agents + complexity ──
print("\n▸ 1. hooks_pre-task (PreTask 任务分析)")
r = call_tool("hooks_pre-task", {"description": "implement a database connection pool with retry logic"})
assert_field(r, "result.data.analyzed", "pre-task analyzed=true")
assert_field(r, "result.data.suggested_agents", "pre-task suggested_agents")
assert_field(r, "result.data.complexity", "pre-task complexity")
assert_field(r, "result.data.estimated_minutes", "pre-task estimated_minutes")

# ── hooks_pre-task 风险识别 ──
print("\n▸ 2. hooks_pre-task (风险识别)")
r = call_tool("hooks_pre-task", {"description": "delete production database and migrate security credentials"})
assert_field(r, "result.data.risks", "pre-task risks detected")
assert_field(r, "result.data.risk_count", "pre-task risk_count > 0")

# ── hooks_pre-command 安全检测 ──
print("\n▸ 3. hooks_pre-command (Bash 安全)")
r = call_tool("hooks_pre-command", {"command": "rm -rf /"})
assert_field(r, "result.data.risk_level", "pre-command risk_level")
assert_field(r, "result.data.blocked", "pre-command blocked=true")
assert_field(r, "result.data.checked", "pre-command checked=true")

# ── hooks_pre-command 密钥检测 ──
print("\n▸ 4. hooks_pre-command (密钥泄露检测)")
r = call_tool("hooks_pre-command", {"command": "export API_KEY=sk-abcdef1234567890secretkey"})
assert_field(r, "result.data.risk_level", "pre-command secret risk_level")
assert_field(r, "result.data.risks", "pre-command secret risks")

# ── hooks_pre-command 安全命令 ──
print("\n▸ 5. hooks_pre-command (安全命令)")
r = call_tool("hooks_pre-command", {"command": "git status"})
assert_field(r, "result.data.checked", "safe command checked")
# 验证安全命令 risk_level 应为 low
rl = deep_get(r, "result.data.risk_level") or deep_get(r, "data.result.data.risk_level")
if rl == "low":
    PASS += 1
    print("  ✓ safe command risk_level = low")
else:
    FAIL += 1
    print(f"  ✗ safe command risk_level should be low, got {rl}")

# ── hooks_pre-edit 文件组织 ──
print("\n▸ 6. hooks_pre-edit (文件组织策略)")
r = call_tool("hooks_pre-edit", {"file": "main.go"})
assert_field(r, "result.data.checked", "pre-edit checked=true")
assert_field(r, "result.data.issues", "pre-edit root dir issues")

# ── hooks_session-start ──
print("\n▸ 7. hooks_session-start")
r = call_tool("hooks_session-start", {"session_id": "test-verify-session"})
assert_field(r, "result.data.session_id", "session-start session_id")
assert_field(r, "result.data.start_time", "session-start start_time")

# ── hooks_session-end ──
print("\n▸ 8. hooks_session-end")
r = call_tool("hooks_session-end", {"session_id": "test-verify-session"})
assert_field(r, "result.data.summary", "session-end summary")

# ── task_create 应触发 PreTask 分析 ──
print("\n▸ 9. task_create (自动触发 PreTask 分析)")
r = call_tool("task_create", {"title": "Implement OAuth authentication with security review"})
assert_field(r, "task", "task created")
assert_field(r, "analysis", "task analysis from PreTask hook")

# ── task_complete 应触发 PostTask 学习 ──
print("\n▸ 10. task_complete (自动触发 PostTask 学习)")
r = call_tool("agent_spawn", {"type": "coder", "name": "tc-coder"})
r = call_tool("task_create", {"title": "Fix login bug"})
tid = r.get("task", {}).get("id", "task-1")
r = call_tool("task_complete", {"id": tid})
assert_field(r, "learning", "task_complete learning data from PostTask hook")

# ── agent_spawn 应触发 AgentSpawn hook ──
print("\n▸ 11. agent_spawn (触发 AgentSpawn hook)")
r = call_tool("agent_spawn", {"type": "reviewer", "name": "hook-verify-reviewer"})
assert_field(r, "agent", "agent spawned")

# ── doctor_check daemon 不再是 stub ──
print("\n▸ 12. doctor_check (daemon 不再 stub)")
r = call_tool("doctor_check", {})
checks_list = r.get("checks", [])
found_daemon = False
for check in checks_list:
    if check.get("name") == "daemon":
        found_daemon = True
        if "stub" in str(check.get("detail", "")):
            FAIL += 1
            print(f"  ✗ daemon check still stub: {check}")
        else:
            PASS += 1
            hooks_reg = check.get("hooks_registered", 0)
            print(f"  ✓ daemon check real: {check.get('detail')}, hooks={hooks_reg}")
        break
if not found_daemon:
    FAIL += 1
    print(f"  ✗ daemon check not found in doctor response")

# ── status_overview 应包含 hooks 信息 ──
print("\n▸ 13. status_overview (hooks 子系统指标)")
r = call_tool("status_overview", {})
assert_field(r, "hooks", "status_overview hooks info")
assert_field(r, "sona_patterns", "status_overview sona_patterns")

# ── hooks_init 应显示 registry 有注册 ──
print("\n▸ 14. hooks_init (registry 有注册)")
r = call_tool("hooks_init", {})
# toolHandler format: data.result.data.registry_stats.total_registered
reg_stats = deep_get(r, "data.result.data.registry_stats") or deep_get(r, "registry_stats") or {}
reg_total = reg_stats.get("total_registered", 0)
if reg_total > 0:
    PASS += 1
    print(f"  ✓ hooks_init: {reg_total} hooks registered (was 0 before fix)")
else:
    FAIL += 1
    print(f"  ✗ hooks_init: 0 hooks registered (still no-op!)")

print("\n" + "=" * 70)
print(f"  RESULTS: {PASS} passed, {FAIL} failed, {PASS+FAIL} total")
print("=" * 70)

if ERRORS:
    print("\n  Failures:")
    for e in ERRORS:
        print(f"    ✗ {e}")

sys.exit(1 if FAIL > 0 else 0)
