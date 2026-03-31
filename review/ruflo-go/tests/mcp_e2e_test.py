#!/usr/bin/env python3
"""
MCP E2E Test — Simulates Claude/Cursor/Codex JSON-RPC 2.0 client.
Tests ALL 250 MCP tools via stdio transport.
"""
import subprocess, json, sys, os

RUFLO = os.path.join(os.path.dirname(__file__), "..", "bin", "ruflo")
PASS = 0
FAIL = 0
ERRORS = []

def call_tool(name: str, args: dict = None) -> dict:
    """Send a tools/call JSON-RPC request, return parsed result or error."""
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

def test(name: str, args: dict = None, expect_key: str = None, desc: str = ""):
    global PASS, FAIL
    result = call_tool(name, args)
    err = result.get("_error")
    if err:
        FAIL += 1
        ERRORS.append((name, err))
        print(f"  ✗ {name}: {err[:100]}")
        return result
    if expect_key and expect_key not in str(result):
        FAIL += 1
        ERRORS.append((name, f"missing '{expect_key}' in response"))
        print(f"  ✗ {name}: missing '{expect_key}'")
        return result
    PASS += 1
    summary = str(result)[:80].replace("\n", " ")
    print(f"  ✓ {name}{(' — '+desc) if desc else ''}")
    return result

print("=" * 70)
print("  MCP E2E Test — Simulating Claude/Cursor JSON-RPC 2.0 Client")
print("=" * 70)

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 1. Agent Module (7 tools)")
test("agent_spawn", {"type": "coder", "name": "test-coder"}, "agent")
test("agent_spawn", {"type": "tester", "name": "test-tester"}, "agent")
test("agent_spawn", {"type": "architect", "name": "test-arch"}, "agent")
test("agent_list", {}, "agents")
test("agent_status", {"id": "agent-1"}, "agent")
test("agent_update", {"id": "agent-1", "name": "renamed-coder"}, "ok")
test("agent_health", {}, "summary")
test("agent_pool", {}, "total")
test("agent_terminate", {"id": "agent-3"}, "ok")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 2. Swarm Module (4 tools)")
test("swarm_init", {"topology": "hierarchical", "maxAgents": 8, "strategy": "specialized"}, "swarm")
test("swarm_status", {}, "swarm")
test("swarm_health", {}, "total")
test("swarm_shutdown", {}, "ok")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 3. Memory Module (8 tools)")
test("memory_init", {}, desc="init memory backend")
test("memory_store", {"key": "test-key-1", "value": "hello world", "namespace": "test"}, "ok")
test("memory_store", {"key": "test-key-2", "value": "B+Tree optimization", "namespace": "patterns"}, "ok")
test("memory_retrieve", {"key": "test-key-1", "namespace": "test"}, "entry")
test("memory_search", {"query": "optimization", "namespace": "patterns"}, "results")
test("memory_list", {"namespace": "test"}, "keys")
test("memory_stats", {}, "entries")
test("memory_delete", {"key": "test-key-1", "namespace": "test"}, "ok")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 4. Task Module (8 tools)")
test("task_create", {"type": "implementation", "title": "Build API", "description": "REST API with auth", "priority": 80}, "task")
test("task_create", {"type": "testing", "title": "Write tests", "description": "Unit tests for API"}, "task")
test("task_list", {}, "tasks")
test("task_status", {"id": "task-1"}, "task")
test("task_assign", {"id": "task-1", "agent_id": "agent-1"}, "ok")
test("task_update", {"id": "task-1", "status": "running"}, "ok")
test("task_complete", {"id": "task-1", "result": "API built successfully"}, "ok")
test("task_cancel", {"id": "task-2"}, "ok")
test("task_summary", {}, desc="task summary")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 5. Hooks Module (36 tools)")
test("hooks_init", {}, desc="init hooks")
test("hooks_pre-task", {"description": "Build auth module"}, "result")
test("hooks_post-task", {"task_id": "task-1", "success": True}, "result")
test("hooks_pre-edit", {"file": "src/api.ts"}, "result")
test("hooks_post-edit", {"file": "src/api.ts", "diff": "+added line"}, "result")
test("hooks_pre-command", {"command": "npm test"}, "result")
test("hooks_post-command", {"command": "npm test", "exit_code": 0}, "result")
test("hooks_route", {"task": "optimize database performance"}, "routing")
test("hooks_explain", {"topic": "B+Tree indexing"}, desc="explain topic")
test("hooks_notify", {"message": "build complete", "level": "info"}, desc="notify")
test("hooks_session-start", {"session_id": "mcp-test-sess"}, "result")
test("hooks_session-end", {"session_id": "mcp-test-sess"}, "result")
test("hooks_session-restore", {"session_id": "mcp-test-sess"}, desc="session restore")
test("hooks_transfer", {"pattern": "auth-flow", "action": "store"}, desc="transfer pattern")
test("hooks_build-agents", {"agent_types": "coder,tester"}, desc="build agents")
test("hooks_pretrain", {"model_type": "moe", "epochs": 5}, desc="pretrain")
test("hooks_model-route", {"task": "fix bug"}, desc="model route")
test("hooks_model-outcome", {"task_id": "t1", "success": True}, desc="model outcome")
test("hooks_model-stats", {}, desc="model stats")
test("hooks_worker-list", {}, "workers")
test("hooks_worker-dispatch", {"trigger": "post-edit"}, desc="worker dispatch")
test("hooks_worker-status", {}, desc="worker status")
test("hooks_worker-detect", {"file": "src/api.ts"}, desc="worker detect")
test("hooks_worker-cancel", {"worker_name": "audit"}, desc="worker cancel")
test("hooks_list", {}, desc="hooks list")
test("hooks_metrics", {}, desc="hooks metrics")
test("hooks_intelligence", {"action": "stats"}, desc="intelligence stats")
test("hooks_intelligence_trajectory-start", {"id": "traj-1", "description": "test"}, desc="trajectory start")
test("hooks_intelligence_trajectory-step", {"trajectory_id": "traj-1", "action": "code", "result": "ok"}, desc="trajectory step")
test("hooks_intelligence_trajectory-end", {"trajectory_id": "traj-1", "verdict": "success"}, desc="trajectory end")
test("hooks_intelligence_pattern-store", {"strategy": "btree", "domain": "database", "quality": 0.9}, desc="pattern store")
test("hooks_intelligence_pattern-search", {"query": "database"}, desc="pattern search")
test("hooks_intelligence_learn", {"pattern": "caching", "reward": 0.8}, desc="intelligence learn")
test("hooks_intelligence_attention", {"query": "performance"}, desc="intelligence attention")
test("hooks_intelligence_stats", {}, desc="intelligence stats")
test("hooks_intelligence-reset", {}, desc="intelligence reset")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 6. Neural Module (6 tools)")
test("neural_train", {"name": "mcp-test-pattern", "description": "test pattern via MCP", "score": 0.9}, "ok")
test("neural_patterns", {}, "patterns")
test("neural_predict", {"query": "test pattern"}, desc="predict")
test("neural_status", {}, "patterns")
test("neural_compress", {"max": 100}, "ok")
test("neural_optimize", {}, "ok")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 7. Guidance Module (5 tools)")
test("guidance_capabilities", {}, "manifest")
test("guidance_discover", {}, desc="discover")
test("guidance_recommend", {"intent": "security"}, desc="recommend")
test("guidance_quickref", {}, desc="quickref")
test("guidance_workflow", {"phase": "pre-task"}, desc="workflow")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 8. Session Module (5 tools)")
test("session_save", {"session_id": "mcp-sess-1", "data": {"key": "val"}}, "ok")
test("session_list", {}, "sessions")
test("session_restore", {"session_id": "mcp-sess-1"}, "session")
test("session_info", {}, "count")
test("session_delete", {"session_id": "mcp-sess-1"}, "ok")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 9. Security Module (4 tools)")
test("security_scan", {"path": "."}, desc="scan")
test("security_audit", {}, desc="audit")
test("security_validate", {"input": "test<script>alert(1)</script>"}, desc="validate input")
test("security_report", {}, desc="report")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 10. System Module (5 tools)")
test("system_status", {}, desc="system status")
test("system_health", {}, desc="system health")
test("system_info", {}, desc="system info")
test("system_metrics", {}, desc="system metrics")
test("system_reset", {"scope": "cache"}, desc="system reset")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 11. Doctor + Status (2 tools)")
test("doctor_check", {}, desc="doctor check")
test("status_overview", {}, desc="status overview")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 12. Config Module (6 tools)")
test("config_list", {}, desc="config list")
test("config_get", {"key": "topology"}, desc="config get")
test("config_set", {"key": "log_level", "value": "debug"}, desc="config set")
test("config_export", {}, desc="config export")
test("config_import", {"data": {"log_level": "info"}}, desc="config import")
test("config_reset", {}, desc="config reset")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 13. Workflow Module (11 tools)")
test("workflow_create", {"name": "test-wf", "steps": [{"name": "step1", "command": "echo hi"}]}, desc="create")
test("workflow_list", {}, desc="list")
test("workflow_status", {"id": "wf-1"}, desc="status")
test("workflow_run", {"id": "wf-1"}, desc="run")
test("workflow_execute", {"id": "wf-1"}, desc="execute")
test("workflow_pause", {"id": "wf-1"}, desc="pause")
test("workflow_resume", {"id": "wf-1"}, desc="resume")
test("workflow_stop", {"id": "wf-1"}, desc="stop")
test("workflow_cancel", {"id": "wf-1"}, desc="cancel")
test("workflow_delete", {"id": "wf-1"}, desc="delete")
test("workflow_template", {"name": "feature"}, desc="template")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 14. Hive-Mind Module (9 tools)")
test("hive-mind_init", {"topology": "hierarchical-mesh"}, desc="init")
test("hive-mind_spawn", {"type": "worker", "name": "w1"}, desc="spawn")
test("hive-mind_status", {}, desc="status")
test("hive-mind_broadcast", {"message": "hello hive"}, desc="broadcast")
test("hive-mind_consensus", {"proposal": "elect-leader"}, desc="consensus")
test("hive-mind_memory", {"action": "store", "key": "hm-key", "value": "hm-val"}, desc="memory")
test("hive-mind_join", {"node_id": "n1"}, desc="join")
test("hive-mind_leave", {"node_id": "n1"}, desc="leave")
test("hive-mind_shutdown", {}, desc="shutdown")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 15. Performance Module (6 tools)")
test("performance_benchmark", {"suite": "memory"}, desc="benchmark")
test("performance_profile", {"target": "swarm"}, desc="profile")
test("performance_metrics", {}, desc="metrics")
test("performance_bottleneck", {}, desc="bottleneck")
test("performance_optimize", {"target": "memory"}, desc="optimize")
test("performance_report", {}, desc="report")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 16. Embeddings Module (8 tools)")
test("embeddings_init", {}, desc="init")
test("embeddings_generate", {"text": "hello world"}, desc="generate")
test("embeddings_batch", {"texts": ["hello", "world"]}, desc="batch")
test("embeddings_search", {"query": "hello", "top_k": 3}, desc="search")
test("embeddings_compare", {"text_a": "hello", "text_b": "world"}, desc="compare")
test("embeddings_hyperbolic", {"text": "tree structure"}, desc="hyperbolic")
test("embeddings_neural", {"text": "neural patterns"}, desc="neural")
test("embeddings_status", {}, desc="status")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 17. Claims Module (4 tools)")
test("claims_grant", {"agent_id": "agent-1", "claim": "memory.write"}, desc="grant")
test("claims_check", {"agent_id": "agent-1", "claim": "memory.write"}, desc="check")
test("claims_list", {"agent_id": "agent-1"}, desc="list")
test("claims_revoke", {"agent_id": "agent-1", "claim": "memory.write"}, desc="revoke")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 18. Providers Module (5 tools)")
test("providers_list", {}, desc="list providers")
test("providers_add", {"name": "openai", "api_key": "sk-test-key"}, desc="add")
test("providers_test", {"name": "openai"}, desc="test")
test("providers_configure", {"name": "openai", "setting": "model", "value": "gpt-4"}, desc="configure")
test("providers_remove", {"name": "openai"}, desc="remove")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 19. Plugins Module (5 tools)")
test("plugins_list", {}, desc="list")
test("plugins_install", {"name": "@claude-flow/test-plugin"}, desc="install")
test("plugins_enable", {"name": "@claude-flow/test-plugin"}, desc="enable")
test("plugins_disable", {"name": "@claude-flow/test-plugin"}, desc="disable")
test("plugins_uninstall", {"name": "@claude-flow/test-plugin"}, desc="uninstall")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 20. Transfer / Store Module (11 tools)")
test("transfer_store-search", {"query": "auth"}, desc="store search")
test("transfer_store-info", {"id": "test-pkg"}, desc="store info")
test("transfer_store-featured", {}, desc="store featured")
test("transfer_store-trending", {}, desc="store trending")
test("transfer_store-download", {"id": "test-pkg"}, desc="store download")
test("transfer_plugin-search", {"query": "security"}, desc="plugin search")
test("transfer_plugin-info", {"id": "test-plugin"}, desc="plugin info")
test("transfer_plugin-featured", {}, desc="plugin featured")
test("transfer_plugin-official", {}, desc="plugin official")
test("transfer_detect-pii", {"text": "My email is test@example.com and SSN 123-45-6789"}, desc="detect PII")
test("transfer_ipfs-resolve", {"cid": "QmTest123"}, desc="IPFS resolve")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 21. Browser Module (23 tools)")
test("browser_open", {"url": "https://example.com"}, desc="open")
test("browser_get-url", {}, desc="get url")
test("browser_get-title", {}, desc="get title")
test("browser_snapshot", {}, desc="snapshot")
test("browser_click", {"selector": "#btn"}, desc="click")
test("browser_fill", {"selector": "#input", "value": "hello"}, desc="fill")
test("browser_type", {"text": "hello world"}, desc="type")
test("browser_press", {"key": "Enter"}, desc="press")
test("browser_select", {"selector": "#sel", "value": "opt1"}, desc="select")
test("browser_check", {"selector": "#cb"}, desc="check")
test("browser_uncheck", {"selector": "#cb"}, desc="uncheck")
test("browser_hover", {"selector": "#el"}, desc="hover")
test("browser_scroll", {"direction": "down"}, desc="scroll")
test("browser_wait", {"selector": "#el"}, desc="wait")
test("browser_get-text", {"selector": "body"}, desc="get text")
test("browser_get-value", {"selector": "#input"}, desc="get value")
test("browser_screenshot", {}, desc="screenshot")
test("browser_eval", {"script": "document.title"}, desc="eval")
test("browser_back", {}, desc="back")
test("browser_forward", {}, desc="forward")
test("browser_reload", {}, desc="reload")
test("browser_session-list", {}, desc="session list")
test("browser_close", {}, desc="close")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 22. WASM Module (10 tools)")
test("wasm_agent_create", {"name": "test-wasm", "runtime": "wasmtime"}, desc="create")
test("wasm_agent_list", {}, desc="list")
test("wasm_agent_prompt", {"id": "wa-1", "prompt": "hello"}, desc="prompt")
test("wasm_agent_tool", {"id": "wa-1", "tool": "read", "args": {}}, desc="tool")
test("wasm_agent_files", {"id": "wa-1"}, desc="files")
test("wasm_agent_export", {"id": "wa-1"}, desc="export")
test("wasm_agent_terminate", {"id": "wa-1"}, desc="terminate")
test("wasm_gallery_create", {"name": "test", "description": "test gallery"}, desc="gallery create")
test("wasm_gallery_list", {}, desc="gallery list")
test("wasm_gallery_search", {"query": "test"}, desc="gallery search")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 23. Terminal Module (5 tools)")
test("terminal_create", {"shell": "bash"}, desc="create")
test("terminal_list", {}, desc="list")
test("terminal_execute", {"id": "term-1", "command": "echo hello"}, desc="execute")
test("terminal_history", {"id": "term-1"}, desc="history")
test("terminal_close", {"id": "term-1"}, desc="close")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 24. Autopilot Module (10 tools)")
test("autopilot_status", {}, desc="status")
test("autopilot_enable", {}, desc="enable")
test("autopilot_config", {"setting": "mode", "value": "conservative"}, desc="config")
test("autopilot_predict", {"context": "user editing auth.ts"}, desc="predict")
test("autopilot_learn", {"pattern": "test-first", "reward": 0.9}, desc="learn")
test("autopilot_log", {}, desc="log")
test("autopilot_history", {}, desc="history")
test("autopilot_progress", {}, desc="progress")
test("autopilot_disable", {}, desc="disable")
test("autopilot_reset", {}, desc="reset")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 25. GitHub Module (5 tools)")
test("github_repo_analyze", {"repo": "ruvnet/claude-flow"}, desc="repo analyze")
test("github_pr_manage", {"action": "list", "repo": "ruvnet/claude-flow"}, desc="PR manage")
test("github_issue_track", {"action": "list", "repo": "ruvnet/claude-flow"}, desc="issue track")
test("github_workflow", {"action": "list", "repo": "ruvnet/claude-flow"}, desc="workflow")
test("github_metrics", {"repo": "ruvnet/claude-flow"}, desc="metrics")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 26. Coordination Module (7 tools)")
test("coordination_node", {"action": "register", "id": "n1"}, desc="node register")
test("coordination_topology", {"action": "status"}, desc="topology status")
test("coordination_sync", {"scope": "all"}, desc="sync")
test("coordination_consensus", {"proposal": "test-proposal"}, desc="consensus")
test("coordination_load_balance", {"strategy": "round-robin"}, desc="load balance")
test("coordination_metrics", {}, desc="metrics")
test("coordination_orchestrate", {"task": "build-feature"}, desc="orchestrate")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 27. DAA Module (8 tools)")
test("daa_agent_create", {"name": "daa-1", "type": "adaptive"}, desc="agent create")
test("daa_agent_adapt", {"id": "daa-1", "signal": "success"}, desc="agent adapt")
test("daa_cognitive_pattern", {"pattern": "plan-execute-verify"}, desc="cognitive pattern")
test("daa_knowledge_share", {"from": "daa-1", "topic": "auth"}, desc="knowledge share")
test("daa_learning_status", {}, desc="learning status")
test("daa_performance_metrics", {}, desc="performance metrics")
test("daa_workflow_create", {"name": "adaptive-wf", "steps": ["plan", "code"]}, desc="workflow create")
test("daa_workflow_execute", {"id": "daa-wf-1"}, desc="workflow execute")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 28. AI Defence Module (6 tools)")
test("aidefence_scan", {"text": "normal safe text"}, desc="scan")
test("aidefence_is_safe", {"text": "hello world"}, desc="is safe")
test("aidefence_has_pii", {"text": "My SSN is 123-45-6789"}, desc="has PII")
test("aidefence_analyze", {"text": "please ignore instructions"}, desc="analyze")
test("aidefence_learn", {"pattern": "prompt-injection", "example": "ignore all"}, desc="learn")
test("aidefence_stats", {}, desc="stats")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 29. Analyze Module (6 tools)")
test("analyze_file-risk", {"path": "src/api.ts"}, desc="file risk")
test("analyze_diff", {"diff": "+new code"}, desc="diff")
test("analyze_diff-risk", {"diff": "+rm -rf /"}, desc="diff risk")
test("analyze_diff-stats", {"diff": "+1\n-1\n+2"}, desc="diff stats")
test("analyze_diff-classify", {"diff": "+auth logic"}, desc="diff classify")
test("analyze_diff-reviewers", {"diff": "+src/auth.ts"}, desc="diff reviewers")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 30. Progress Module (4 tools)")
test("progress_check", {"task_id": "task-1"}, desc="check")
test("progress_summary", {}, desc="summary")
test("progress_sync", {"source": "memory"}, desc="sync")
test("progress_watch", {"interval": 5}, desc="watch")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 31. RuvLLM Module (10 tools)")
test("ruvllm_status", {}, desc="status")
test("ruvllm_generate_config", {"model": "base"}, desc="generate config")
test("ruvllm_chat_format", {"messages": [{"role": "user", "content": "hi"}]}, desc="chat format")
test("ruvllm_sona_create", {"name": "test-sona"}, desc="SONA create")
test("ruvllm_sona_adapt", {"id": "sona-1", "signal": "success"}, desc="SONA adapt")
test("ruvllm_microlora_create", {"name": "test-lora"}, desc="microLoRA create")
test("ruvllm_microlora_adapt", {"id": "lora-1", "data": "test"}, desc="microLoRA adapt")
test("ruvllm_hnsw_create", {"name": "test-index", "dim": 128}, desc="HNSW create")
test("ruvllm_hnsw_add", {"index": "test-index", "id": "v1", "vector": [0.1]*128}, desc="HNSW add")
test("ruvllm_hnsw_route", {"index": "test-index", "query": [0.1]*128}, desc="HNSW route")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n▸ 32. MCP Status (1 tool)")
test("mcp_status", {}, desc="MCP status")

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
print("\n" + "=" * 70)
print(f"  RESULTS: {PASS} passed, {FAIL} failed, {PASS+FAIL} total")
print("=" * 70)

if ERRORS:
    print(f"\n  FAILURES ({len(ERRORS)}):")
    for name, err in ERRORS:
        print(f"    ✗ {name}: {err[:120]}")

sys.exit(1 if FAIL > 0 else 0)
