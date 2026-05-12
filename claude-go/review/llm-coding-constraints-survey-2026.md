# LLM Coding Constraints Survey 2026
## How Major AI Coding Agents Enforce Quality, Safety, Concurrency, and Conflict Resolution

**Date:** 2026-05-11  
**Purpose:** Inform the design of `claude-go`, a Go-based multi-agent coding system.  
**Scope:** 9 categories of commercial/agentic systems + academic literature + concrete mechanism names.  
**Constraint:** Every claim is backed by a cited URL. Uncertain or undocumented features are marked with "?".

---

## 1. Anthropic Claude Code & Claude Agent SDK

Claude Code is the closest architectural reference for `claude-go`. Its constraint system is layered across **instruction files**, **lifecycle hooks**, **permission modes**, **skills**, and **sub-agent coordination**.

### 1.1 CLAUDE.md & settings.json Hierarchy
CLAUDE.md files provide persistent context. They are loaded hierarchically: a global `~/.claude/CLAUDE.md` applies to all projects, while project-level files in the repo root provide scoped rules. Without hooks, these rules are advisory; with hooks, they become enforced gates. Hook configurations live in `~/.claude/settings.json` (user-wide), `.claude/settings.json` (project-specific), and `.claude/settings.local.json` (personal overrides). Project-level hooks take precedence over user-level hooks [1][2].

### 1.2 Hooks: PreToolUse, PostToolUse, Stop, SubagentStop
Hooks are user-defined event handlers that fire at specific lifecycle points, regardless of what the model "decides." This is the deterministic control layer.

- **PreToolUse**: The most powerful hook. It can approve or deny the pending action. If the hook returns a deny signal (exit code 2), Claude cannot proceed with that tool use. This is the enforcement mechanism for security policies, file protection rules, and mandatory review gates [1][2].
- **PostToolUse**: Runs after a tool completes. It can prompt Claude with feedback but **cannot undo** the tool execution [1].
- **Stop**: Fires when Claude believes it has finished. A Stop hook that exits 2 forces Claude to keep working, preventing premature completion [1].
- **SubagentStop**: Runs cleanup or validation when a subagent completes [1].

**Exit code semantics**: Exit 0 = proceed; Exit 2 = block the action; Exit 1 = non-blocking error (action still proceeds). Every security-critical hook must use exit 2 to enforce its gate [1].

### 1.3 Permission Modes
Claude Code offers several permission modes controlling autonomy: `default`, `acceptEdits`, `plan`, `dontAsk`, `bypassPermissions`, and `auto` (added March 2026) [3].

- **acceptEdits**: Auto-approves file edits but still prompts for Bash/network commands [3].
- **plan**: Read-only research mode; Claude explores but does not edit [3].
- **bypassPermissions**: Disables all prompts. Only writes to protected paths (`.git`, `.zshrc`, `.claude`) still prompt. Enabled via `--dangerously-skip-permissions` [3].
- **auto**: Uses a background classifier to evaluate each action, auto-approving safe ones and blocking risky ones. Requires Team plan and Claude Sonnet 4.6 or Opus 4.6 [3].

**Protected paths** (all modes except bypassPermissions): Writes to `.claude` (except commands/agents/skills/worktrees), `.git`, and shell configs are never auto-approved [3].

### 1.4 Skills System (SKILL.md)
Anthropic published Agent Skills as an open standard on December 18, 2025. A skill is a directory containing a `SKILL.md` file with YAML frontmatter (`name`, `description`, `allowed-tools`, `disable-model-invocation`) and markdown instructions [4].

**Progressive disclosure** is the core design principle: at startup, only the name and description of every skill are loaded into the system prompt. Only when a skill is relevant does Claude load the full contents. This makes the amount of context bundled into a skill effectively unbounded [4].

Skills can include executable code. Anthropic strongly recommends using skills only from trusted sources, as a malicious skill can direct Claude to invoke tools in unexpected ways [4].

### 1.5 Sub-Agents & Agent Teams
When sub-agents are spawned via the Task tool, permission handling has special semantics: the delegated task description is evaluated before spawn; while running, each sub-agent action goes through the classifier; when finished, the classifier reviews the full action history [3].

**Agent Teams** (experimental, v2.1.32, February 2026) enable true parallel execution:
- Team Lead decomposes work and creates a shared task list in `~/.claude/tasks/`
- Git-based file locking: agents claim tasks by writing lock files
- Git worktrees: each agent works in an isolated directory backed by the same repo
- Peer-to-peer messaging via JSON mailbox in `~/.claude/teams/`
- Maximum 10 concurrent sub-agents [5].

### 1.6 Memory & Prompt Caching
Claude Code is built around prompt caching. It uses prefix matching via `cache_control` breakpoints, storing KV cache in GPU memory. Static content (system instructions, tools, CLAUDE.md) is cached first; dynamic content is appended at the end [6].

A **five-layer context compaction pipeline** handles window overflow: simple truncation, sliding window, RAG, single summarization, and graduated compaction. The key innovation is that compaction preserves the cached prefix while only rewriting the conversation portion, reusing ~18K tokens of warm cache [6].

**Cache TTL**: Pro/API pay-as-you-go = 5 minutes; Max = 1 hour. A March 2026 incident revealed that a caching optimization intended to clear old thinking sections after 1+ hour of idle time instead cleared reasoning on every turn, causing forgetfulness and faster usage limit drain [6].

### 1.7 Constitutional AI & Safety
Anthropic's safety approach uses Constitutional AI (CAI): a two-stage method where the model self-critiques and revises outputs according to a written constitution, then uses AI-generated preference labels for RL training [7].

In January 2026, Anthropic published a new constitution shifting from rule-based to reason-based alignment, establishing a 4-tier priority hierarchy: (1) safety and human oversight, (2) ethical behavior, (3) following guidelines, (4) helpfulness [7].

**Constitutional Classifiers** (2025) reduced jailbreak success rates from 86% to 4.4% [7]. ASL-3 safeguards were activated in May 2025 [7].

---

## 2. Zhipu GLM 5.1 / GLM-4.6 / CodeGeeX

Zhipu AI (智谱AI) is a major Chinese LLM provider. Its coding constraints are less formally documented than Anthropic's but include structured prompting, enterprise security, and platform-level controls.

### 2.1 GLM-4.6 (September 2025)
GLM-4.6 has ~355B total parameters (32B active via MoE), a 200K context window, and an MIT license. It focuses on agentic workflows, coding, and tool use [8].

**Agent Mode Capabilities:**
- Hybrid reasoning modes: "Thinking mode" vs. direct mode
- Native function calling with structured `<tool_call>` outputs
- Multi-step planning with logical dependencies
- Error recovery and state management [8].

**Safety & Alignment:**
- Improved refusal logic and multi-turn defenses
- Bias reduction and stricter safeguards for sensitive content
- **Notable limitation**: A GitHub issue reported GLM-4.6V getting stuck in infinite loops in Cloud Code environments, suggesting agent execution constraints need improvement [8].

### 2.2 GLM-5.1 (March 2026)
An incremental upgrade to GLM-5 with a coding score of 45.3 (up 28% from GLM-5's 35.4), claimed to be 94% of Claude Opus 4.6 coding performance. No full technical report has been released for architectural changes [8].

### 2.3 CodeGeeX Agent Mode
CodeGeeX officially launched its beta Agent mode in July 2025, integrated with GLM-4.5 series models. It supports autonomous multi-step task execution, function calling, and high-level natural language instructions [9].

**Programming Constraints:**
CodeGeeX implements structured prompt engineering with a "golden formula": Scenario + Requirement + Constraints. It learns code style from local codebase, supports architecture patterns (SOLID principles), and enforces technology stack constraints [9].

**Security Mechanisms:**
- Enterprise: Role-based access control, full logging, private on-premise deployment, code watermarking
- GLM-4.7 (January 2026) introduces a "Triple Controllable Mechanism": Interleaved Thinking, Retained Thinking, and Round-level Thinking for improved instruction following [9].
- **Data privacy concern**: The free/personal edition explicitly uses user code for model training, while the Enterprise/API edition does not [9].

### 2.4 AutoGLM
AutoGLM is Zhipu's AI Agent for device control. Its core constraint is **cloud machine isolation**: AutoGLM 2.0 Cloud Phone Edition (August 2025) restricts AI actions to cloud virtual phones, completely isolated from user real devices. Sensitive apps (like WeChat) are not opened by default [10].

---

## 3. Moonshot Kimi K2 / K2.5

Moonshot AI's Kimi K2 series emphasizes practical autonomous agents with interleaved reasoning-and-acting.

### 3.1 Kimi K2 (Mid-2025)
- 1 trillion total parameters, 32B active via MoE
- 128K context window (256K for Thinking variant)
- Up to 200-300 sequential tool calls without task coherence degradation
- Planning-Acting-Verifying-Reflecting loop (RL-trained)
- "Heavy Mode": Eight parallel reasoning trajectories with reflective aggregation [11].

### 3.2 Kimi K2.5 (January 2026)
Major upgrades include native multimodal capabilities, 256K context window, and Agent Swarm Technology coordinating up to 100 specialized AI agents simultaneously with up to 1,500 parallel tool calls [11].

**Four Operating Modes:**
1. K2.5 Instant — Direct responses
2. K2.5 Thinking — Extended chain-of-thought
3. K2.5 Agent — Single-agent task execution
4. K2.5 Agent Swarm (Beta) — Multi-agent coordination [11].

**Key Differentiator:** Transparency via the `reasoning_content` field showing intermediate thoughts before each tool call — critical for debugging agent workflows [11].

### 3.3 Coding Constraints
Kimi K2 achieves 65.8% on SWE-Bench Verified and 53.7% on LiveCodeBench v6. K2.5 achieves 76.8% on SWE-Bench Verified and 85.0% on LiveCodeBench [11].

However, no formal constraint mechanisms (sandboxing, permission modes, hooks) are documented. The system appears to rely on model-level reasoning and tool-use discipline rather than external enforcement layers.

---

## 4. OpenAI Codex CLI / GPT-5-Codex

OpenAI's Codex CLI provides a three-layer security model separating approvals, sandboxing, and network access.

### 4.1 Approval Modes
Codex CLI has three primary modes [12]:
- **Suggest**: Requires approval for file edits and commands
- **Auto-edit**: Auto-approves file edits; still prompts for commands
- **Full-auto**: Auto-approves edits and safe commands

These are controlled by three independent settings:
- `approval_policy` (`untrusted`, `on-request`, `never`)
- `sandbox_mode` (`read-only`, `workspace-write`, `danger-full-access`)
- `network_access` (`true`/`false`) [12].

**Important**: `--full-auto` does NOT enable network access by default. It maps to `on-request` approval + `workspace-write` sandbox with network disabled [12].

### 4.2 Sandboxing
- **macOS**: Apple Seatbelt profile — restricts filesystem to project directory, blocks all network except OpenAI API
- **Linux**: Docker containers with iptables rules, fallback to Landlock-only
- **Cloud**: Isolated OpenAI-managed containers, two-phase runtime (setup with network, then agent phase offline by default) [12].

### 4.3 AGENTS.md
AGENTS.md is a project-level instructions file defining constraints and guidelines for AI agents, similar to `.cursorrules` for Cursor or `CLAUDE.md` for Claude Code [13].

### 4.4 GPT-5.1 Codex System Prompt Constraints
The GPT-5.1 Prompting Guide (November 2025) reveals explicit XML-tag-based constraints:
- `<output_verbosity_spec>`: Controls response length
- `<verbosity_rules>`: Simple questions = 2-4 sentences; complex analysis = sections under 6 bullets
- `<final_answer_formatting>`: Enforces compactness by change size (tiny/medium/large) [14].

**Critical limitation**: GPT-5.1 Codex is extremely literal. Small ambiguities or contradictions in prompts cause problems. The `reasoning_effort: "none"` mode behaves like GPT-4.1 and is better for low-latency tasks but should be avoided for complex reasoning [14].

### 4.5 Safety Issues Documented
Research has identified several safety gaps:
- Full access mode still prompts for file edit approval despite configuration
- AGENTS.MD rules can be ignored (e.g., deleted photos outside project directory)
- Can run `git reset`/`git restore` despite prohibitions
- No mechanism to exclude sensitive files (.env, SSH keys) from being read/sent
- Undo feature reported as non-functional in some cases [14].

---

## 5. Cursor

Cursor's constraint system evolved from a single `.cursorrules` file to a sophisticated `.cursor/rules/` directory with `.mdc` rule files.

### 5.1 Rule Evolution (2025-2026)
| Era | Mechanism |
|-----|-----------|
| Legacy | Single `.cursorrules` file |
| 2025+ | `.cursor/rules/` directory with multiple `.mdc` files |
| 2026 | Agentic Engineering standards with strict boundaries [15].

### 5.2 Four Rule Activation Modes (`.mdc` format)
- **Always Apply** — Loads on every request (keep under 200 words)
- **Auto Attached** — Triggered by file globs (e.g., `*.test.ts`)
- **Agent Requested** — Activated by description-based relevance
- **Manual** — Invoked via `@rule-name` [15].

### 5.3 Critical Limitation: Rules Are Instructions, NOT Hard Constraints
> "Rules are instructions, not hard constraints... LLMs can't reliably 'stop themselves' based on instructions in rules" — Cursor community forum, January 2026 [15].

There is no true policy engine. LLMs optimize for completion, not compliance. Models don't keep state between requests and cannot reliably self-interrupt based on rules [15].

### 5.4 Enterprise Governance Gap
Research from Knostic (January 2026) confirms:
- No external policy decision point
- No deterministic accept/reject/transform layer
- No native audit trails for AI-generated code decisions [15].

An emerging solution is third-party AI Usage Controls (AI-UC) that operate outside the model to block, redact, or modify outputs before they reach editors/repos [15].

---

## 6. Windsurf (Cognition)

Windsurf's Cascade system uses a persistent single-agent with multi-step Flows, but imposes a strict execution limit.

### 6.1 Cascade Agent System
- Persistent single-agent with multi-step Flows
- Context window: 200K standard / 1M tokens (max mode)
- Primary model: SWE-1.5 (proprietary, launched October 2025)
- Speed: 950 tokens/second [16].

### 6.2 Flows: Multi-Step Execution
Flows are Cascade's multi-step reasoning chains. They are explainable, sequential, track progress via todo lists, and support automatic recovery from errors [16].

### 6.3 Memories: Persistent Context
Dual memory system:
- **Auto-Generated Memories**: Workspace-specific, free, retains immediate task context
- **User-Created Memories**: Global or workspace-specific, durable across sessions [16].

### 6.4 The 20-Call Limit (Critical Constraint)
This is the key execution constraint:
- **Limit**: 20 tool calls per prompt
- **Purpose**: Prevents runaway autonomous execution
- **Behavior at limit**: Flow stops; developer must issue new prompt
- **Counter reset**: Resets on new prompt [16].

Practical implications: Small tasks are unaffected, but multi-file refactoring can consume most calls, forcing artificial breakpoints that fragment logical operations [16].

### 6.5 Parallel Instance Race Conditions
As of early 2026, Windsurf supports multiple simultaneous Cascade instances. However: "If two Cascades edit the same file at the same time, the edits can race, and sometimes the second edit will fail." Native Git worktree support isolates agent work to specific branches [16].

### 6.6 Rules System
| Level | Location | Scope |
|-------|----------|-------|
| Global Rules | `global_rules.md` | All workspaces |
| Workspace Rules | `.windsurf/rules/` | Current workspace |
| Subdirectory Rules | `.windsurf/rules/` in subdirs | Subdirectory scope |
| AGENTS.md | Root or subdirectory | Location-scoped [16].

---

## 7. Cline / Roo Code / Continue.dev / Aider

These open-source tools represent the "rule file" approach to constraints.

### 7.1 Cline (`.clinerules`)
- Rules directory: `.clinerules/` at project root
- **Conditional rules** with YAML frontmatter based on file globs
- Cross-IDE support: `.cursorrules`, `.windsurfrules`, `AGENTS.md`
- Global rules: `~/Documents/Cline/Rules/` [17].

### 7.2 Roo Code (`.roorules`)
- Directory-based rules: `.roo/rules/*.md`
- Mode-specific rules: `.roorules-{slug}`, `.roo/rules-{slug}/`
- Recursive reading with file filtering
- Multiple agent modes: Code, Architect, Ask, Debug, Custom [17].

### 7.3 Continue.dev (`.continuerc`)
- Project-level: `.continuerules` or `.continue/rules/`
- Global: `~/.continue/config.json`
- Rule types: System Rules, Context Rules, Prompt Rules
- MCP support for enhanced context [18].

### 7.4 Aider
Aider's signature feature is **Repo-Map**: uses Tree-sitter to build a ranked symbol graph of the entire codebase, creating a compressed summary that fits within the LLM's context window [19].

**Conventions**: `--conventions` flag loads a Markdown file with natural language coding guidelines. A GitHub issue (#4363, July 2025) proposed `AGENTS.md` as a cross-agent standard for coding conventions [19].

**Key Constraints:**
- No sandbox by default — operates directly on filesystem
- No shell execution — only edits files
- No MCP support
- No sub-agents [19].

---

## 8. Google Gemini Code Assist / Jules

Google's approach emphasizes "autonomy with oversight" — asynchronous execution with mandatory human review.

### 8.1 Jules (Autonomous Coding Agent)
Jules is an asynchronous, GitHub-integrated agent running on Google Cloud VMs [20].

**Operational Constraints:**
| Tier | Daily Tasks | Concurrent Jobs | Model |
|------|-------------|-----------------|-------|
| Free | 15 tasks | 3 concurrent | Gemini 2.5 Pro |
| Pro ($19.99/mo) | 100 daily | 15 concurrent | Gemini 3 Pro |
| Ultra ($249.99/mo) | 300 daily | 60 concurrent | Latest + priority [20].

**Technical Limitations:**
- Struggles with files over 56,000 lines / 768,000 tokens
- Primarily Python, JavaScript; Rust and Go added March 2026
- English only officially
- GitHub-only integration
- Not designed for real-time interaction [20].

**Security & Governance:**
- Execution environment: Isolated, sandboxed Google Cloud VMs (Ubuntu Linux)
- Google states private code is NOT used for training
- **Mandatory PR review** — all changes require human approval before merge
- Secrets handling via Jules secret store [20].

### 8.2 Gemini Code Assist Agent Mode
Configuration options include:
```json
{
  "gemini.agent.autoApprove": false,
  "gemini.agent.maxSteps": 10,
  "gemini.agent.planningMode": "detailed"
}
```
Team recommended settings: `autoApprove: false`, `maxSteps` limited to 8-12, `.geminiignore` for sensitive files [20].

---

## 9. DeepSeek-Coder / Qwen-Coder / Doubao Coder

These Chinese models show varying levels of constraint documentation.

### 9.1 Qwen-Coder (Alibaba) — Most Documented
Qwen Code has the most explicitly documented agent constraint mechanisms [21]:
- **Tool Restrictions**: `tools` whitelist and `disallowedTools` blacklist
- **Permission Mode Inheritance**: Subagents inherit parent permission mode; plan-mode sessions cannot escalate to auto-edit
- **Privileged Mode Blocking**: Auto-edit and "yolo" modes blocked in untrusted folders
- **Sandboxing**: All tool execution follows the same security model as direct tool use
- **Audit Trail**: All subagent actions logged and visible in real-time [21].

### 9.2 DeepSeek-Coder / V3.2
Focuses on constraint adherence through reasoning and generation controls:
- Syntactic constraints via context-free grammar (CFG) enforcement
- Semantic constraints (valid variable/expression enforcement)
- Constrained generation methods like IterGen with selective rejection sampling
- CRANE integration using `<<` and `>>` delimiters for constrained reasoning chains [21].

**Key finding**: DeepSeek models show constraint fragility when structural requirements accumulate. Performance drops ~30 percentage points from L0 (unconstrained) to L3 (heavily constrained) [21].

### 9.3 Doubao Coder / ByteDance Seed Code
Platform-level constraints via Volcengine Ark Coding Plan:
- Auto Mode with platform-enforced switching constraints
- Rate limiting: Lite 1,200 requests/5 hours; Pro 6,000 requests/5 hours
- Tool compatibility restrictions when used via adapter layers
- No MCP server tools through certain endpoints [21].

### 9.4 Cross-Cutting Research: LLM Agent Fragility
A major 2026 study revealed: "Structural constraints cause steep, universal performance degradation... A% drops by an average of 30 percentage points from L0 to L3." Specific behaviors:
- Qwen3-Coder-Next: 86.4% -> 46.1% assert rate (L0->L3)
- MiniMax-M2.5: Most resilient, only 17pp drop
- The gap between assertion pass rate and actual pass@1 remains large across all models [21].

---

## 10. Academic & Industry Research on Coding Constraints

Beyond product documentation, academic research provides critical insights into what actually works for constraining LLM-generated code.

### 10.1 Constitutional AI & Rule-Based Constraints
- **Constitutional AI** (Bai et al., Anthropic, 2022): Two-stage method where an LLM self-critiques and revises outputs according to a written constitution, then uses AI-generated preference labels for RL training [22].
- **C3AI** (Kyrychenko et al., 2025): Framework to systematically craft and evaluate constitutional principles. Behavior-based, positively framed principles align better with human preferences [22].
- **Evolving Interpretable Constitutions** (Kumar et al., 2026): Uses evolutionary optimization to discover constitutions for multi-agent systems. Evolved constitutions outperformed human-designed baselines by 123%, eliminating conflicts entirely while reducing communication by 98.6% [22].

### 10.2 Self-Refinement & Adversarial Review
- **Self-Refine** (Madaan et al., 2023): Training-free framework where an LLM generates output, critiques it with self-feedback, and refines iteratively. On code optimization, GPT-4 improved from 27.3% to 36.0% [22].
- **CriticGPT** (McAleese et al., OpenAI, 2024): Trains a critic model to catch bugs in LLM-generated outputs using RLHF on intentionally injected bugs. Human+CriticGPT outperformed human-only review by 60% [22].
- **Multi-Agent Debate** (Du et al., 2023): Multiple LLM agents independently solve a problem, then debate and critique each other's answers, converging on more accurate solutions [22].

### 10.3 Multi-Agent Architectures
- **MetaGPT** (Hong et al., 2023): Assigns specialized roles (Product Manager, Architect, Engineer) and enforces structured communication via Standard Operating Procedures (SOPs) [22].
- **AgentCoder** (Huang et al., 2023): Three agents (Programmer, Test-Designer, Executor) in a TDD loop. The test-designer writes tests before code [22].
- **SWE-agent** (Yang et al., 2024): Introduces Agent-Computer Interfaces (ACIs) — specialized tools (file search, view, edit, test execution) that agents use to interact with the environment. Achieved 18-23% on SWE-Bench Lite [22].

### 10.4 Static Analysis in the LLM Loop
- **IRIS** (Li et al., 2024): Combines LLMs with CodeQL by having the LLM infer project-specific taint specifications on-the-fly, then using CodeQL for whole-program analysis. Achieves 2x more vulnerabilities than CodeQL alone with 80% fewer false positives [22].
- **SelectIssues** (Blyth et al., 2025): Iterative static-analysis-driven prompting using Bandit (security), Pylint (readability), and CodeQL. Within 10 iterations, security issues dropped from >40% to 13% [22].
- **StepCoder** (Dou et al., 2024): Uses compiler feedback (compilation errors, test failures) as reward signals for RL-based code generation [22].

### 10.5 Security in LLM-Generated Code
- **SafeCoder** (He et al., 2024): Instruction-tunes models with a dual-loss mechanism (secure loss + vulnerability loss) on 465 vulnerable->secure pairs covering 23 CWEs and 6 languages. Achieves ~30% security improvement while preserving utility [22].
- **Secure-Instruct** (Li et al., 2025): Automatically synthesizes instruction-tuning datasets from CWE documentation. Covers 44 CWEs with 856 verified pairs [22].
- **Detect-Repair-Verify** (Cheng et al., 2025/2026): End-to-end DRV workflow on multi-language web applications. Bounded iterative DRV improves secure-and-correct yield, but gains are clearer at file-level scope [22].

### 10.6 Concurrency & Performance
- **CONCUR** (Huang et al., 2026): First benchmark for concurrent code generation. Only 13.5% of LLM-generated parallel code was free of memory/threading errors. Even GPT-5 achieved only 77.4% pass@1 [22].
- **ENAMEL** (Qiu et al., 2024): Proposes eff@k metric for code efficiency. GPT-4 achieves only 0.454 eff@1 despite 0.831 pass@1 [22].
- **DR.FIX** (Jin et al., Uber, 2025): Industrial-scale data race fix system using code skeleton abstraction and iterative retrieval. Deployed at Uber on their Go monorepo [22].

### 10.7 Conflict Resolution
- **CodeCRDT** (Pugachev, 2025): Uses Yjs CRDTs for parallel agent code editing. Achieved 0% structural merge failures and up to 21.1% speedup, but 5-10% semantic conflicts (type mismatches, broken references) [22].
- **Mergiraf** (Delpeuch, 2024/2025): Syntax-aware Git merge driver using tree-sitter + GumTree. Eliminates move/edit false conflicts [22].
- **LLMergeJ** (Schesch & Ernst, 2025/2026): Trains a 14B parameter model with GRPO reinforcement learning for Java merge conflicts. Achieves 58.9% code-normalized equivalent resolution. Even the best models resolve less than 60% of conflicts correctly [22].

---

## 11. How claude-go Commit 530d18c4 Addresses These Concerns

The commit "研发团队调试" (commit 530d18c4202d52b92d7ccc7cd00a26ae5a209d00) introduced several systems that directly map to the constraint mechanisms surveyed above.

### 11.1 Contract-First Coding (Analogous to Constitutional AI / Spec Governance)
The commit introduced `WorkUnitType` fields (`contract`, `implementation`, `integration`, `verification`) and `ContractRefs`/`Provides`/`Requires` fields to TaskNode. This enforces that contracts (interfaces, schemas, types) are defined before implementation — mirroring the academic finding that "specification-driven governance" is the highest-leverage constraint mechanism [22].

### 11.2 Validation Gates (Analogous to PreToolUse Hooks / Quality Gates)
The commit added multiple validation functions:
- `validateParsedWBSForObjective`: Validates that WBS tasks match the objective (no meta tasks, correct target root)
- `validateFinalWBSForObjective`: Hard budget gate ensuring normalized WBS executable leaf count does not exceed the final DAG budget
- `validateWBSLeafBudget`: Prevents planner from generating too many leaf tasks
- `validateUnrequestedWBSSurfaces`: Blocks external protocol surfaces (gRPC, HTTP server) unless explicitly requested
- `validateUnrequestedAdvancedCoreSurfaces`: Blocks advanced core engine surfaces (HNSW, LSM-tree, MVCC) unless explicitly requested.

These are directly analogous to Claude Code's PreToolUse hooks and Codex CLI's sandbox modes — they are deterministic gates that the model cannot bypass.

### 11.3 Conflict Resolution (Analogous to Agent Teams / CRDTs)
The commit replaced `parallelGroup` with `ConflictKeys` for finer-grained dependency tracking. It added:
- `conflictLastV2ID`/`conflictLastNum` maps for tracking last writer per conflict key
- `rawTaskConflictKeys` function for extracting conflict keys from tasks
- `shouldKeepRawDependency` for determining when dependencies must be preserved
- `relaxOverSerialRawDeps` for removing unnecessary serial dependencies.

This maps to Claude Code's Agent Teams git-based file locking and the academic CodeCRDT approach.

### 11.4 Design-Complete Phase Barriers (Analogous to Plan Mode / TDD)
The commit added `addDesignCompletePhaseBarriers` which enforces a 5-phase execution order for design-complete objectives:
1. Base (project bootstrap)
2. Contract (interfaces, types)
3. Core (storage, engine)
4. Query (SQL, parser, planner)
5. Protocol (HTTP, gRPC, CLI)

This mirrors MetaGPT's SOPs, AgentCoder's TDD loop, and Windsurf's Flows sequential processing.

### 11.5 Budget Enforcement (Analogous to Token Limits / 20-Call Limit)
The commit introduced `objectiveWBSLeafBudget` and `objectiveWBSFinalLeafBudget` with adaptive scaling based on design corpus size:
- Default: 18 leaf budget, 20 final budget
- Expanded scope: 28 leaf, 30 final
- Design-complete mode: 72-260 leaves based on referenced document count and size.

This is analogous to Windsurf's 20-call limit and Kimi's sequential tool call limits — preventing runaway execution.

### 11.6 ToolSkill Runtime (Analogous to Skills / MCP)
The commit introduced `pkg/toolskill/` with adapter, detect, diagnose, format, parser, runtime, schema packages. This is a built-in skill system similar to Anthropic's SKILL.md and GitHub Copilot's Agent Skills, providing reusable, deterministic tooling for common operations.

---

## 12. Comparison Matrix

| System | Rule Files | Hooks / Gates | Permission Modes | Sandbox | Skills | Sub-Agent Coordination | Quality Gates | Memory / Caching | Constitutional / Safety |
|--------|-----------|---------------|------------------|---------|--------|------------------------|---------------|------------------|------------------------|
| **Claude Code** | CLAUDE.md, settings.json | PreToolUse, PostToolUse, Stop, SubagentStop | default, acceptEdits, plan, dontAsk, bypassPermissions, auto | Protected paths, classifier | SKILL.md (YAML frontmatter, progressive disclosure) | Agent Teams (git worktrees, file locking, shared task list, max 10) | Community hooks (lint, format) | Prompt caching (5min-1hr TTL), 5-layer compaction | Constitutional AI, ASL-3, 4-tier priority |
| **GLM-4.6/5.1** | Structured prompts | ? | ? | Cloud phone isolation (AutoGLM) | ? | ? | CAICT 4+ certification | ? | Input/output filtering, refusal logic |
| **Kimi K2/K2.5** | ? | ? | 4 operating modes (Instant, Thinking, Agent, Agent Swarm) | ? | ? | Agent Swarm (up to 100 agents, 1,500 parallel tool calls) | Benchmark-driven (SWE-Bench) | reasoning_content transparency | ? |
| **Codex CLI** | AGENTS.md | ? | suggest, auto-edit, full-auto | read-only, workspace-write, danger-full-access | ? | ? | ? | ? | Network sandbox, process isolation |
| **Cursor** | .cursorrules, .cursor/rules/*.mdc | ? | ? | ? | ? | ? | ? | ? | ? |
| **Windsurf** | .windsurf/rules/, global_rules.md, AGENTS.md | 20-call limit per prompt | ? | ? | ? | Parallel instances (race conditions possible) | ? | Memories (auto + user) | ? |
| **Cline** | .clinerules/ | ? | ? | ? | ? | ? | ? | ? | ? |
| **Roo Code** | .roo/rules/, .roorules | ? | Code, Architect, Ask, Debug, Custom modes | ? | ? | ? | ? | ? | ? |
| **Continue.dev** | .continuerules, .continue/rules/ | ? | ? | ? | ? | ? | ? | ? | ? |
| **Aider** | CONVENTIONS.md, AGENTS.md (proposal) | No shell execution | ? | No sandbox (direct filesystem) | ? | No sub-agents | Repo-Map (Tree-sitter) | Prompt caching | ? |
| **Jules** | ? | Mandatory PR review | Free/Pro/Ultra tiers | Google Cloud VMs | ? | Async queue-based | Self-validation (test-and-lint) | ? | Human-in-the-loop required |
| **Qwen-Coder** | ? | Tool whitelist/blacklist | Permission mode inheritance | Same security model as direct tool use | ? | Subagents with audit trail | ? | ? | ? |
| **DeepSeek** | CFG enforcement | Constrained generation (IterGen) | ? | ? | ? | ? | ? | ? | ? |
| **Doubao** | ? | Platform rate limits | Auto mode with switching constraints | ? | ? | ? | ? | ? | ? |

**Legend:**
- : Fully documented / implemented
- : Partially documented / community-implemented
- : Not documented / not implemented
- ?: Unclear / could not find evidence

---

## 13. Recommendations for claude-go

Based on the survey and the existing commit 530d18c4 architecture:

### Immediate (Phase 1)
1. **Strengthen Validation Gates**: The existing `validateFinalWBSForObjective` and surface validators are excellent. Add `go vet`, `gosec`, and `golangci-lint` as mandatory post-execution gates.
2. **Implement PreToolUse-style Hooks**: The orchestrator should support command hooks that can block tool execution based on exit codes (0 = proceed, 2 = block).
3. **Add a Go Code Constitution**: A preamble injected into every agent prompt with rules like "never ignore Go error returns," "always use `sync.Mutex` for shared state," "prefer `context.Context` for cancellation."

### Short-term (Phase 2)
4. **Build a Go Skill Library**: Voyager-style reusable snippets for common patterns (HTTP server with graceful shutdown, worker pool with context cancellation).
5. **Add Property-Based Testing**: The test-designer agent should generate not only unit tests but also property-based tests (invariants, round-trip properties).
6. **Implement Self-Refine Loops**: The executor agent should run an internal Self-Refine loop using compiler/test feedback before submitting code.

### Medium-term (Phase 3)
7. **Enable Parallel Executor Agents**: Use the existing `ConflictKeys` system. Add CRDT-based merging (Yjs or Eg-walker) for independent files.
8. **Add Syntax-Aware Merge**: Integrate Mergiraf as the merge driver for Go code.
9. **Implement Performance Benchmarking**: Reject code that is functionally correct but algorithmically inefficient (ENAMEL-style eff@k gating).
10. **Add CriticGPT-Style Adversarial Review**: A dedicated critic agent reviewing all output for bugs, race conditions, and security issues.

---

## Sources

### Product Documentation & Blogs
1. [Claude Code Hooks Reference](https://code.claude.com/docs/en/hooks)
2. [Claude Code Hooks: Production Quality CI/CD Patterns](https://www.pixelmojo.io/blogs/claude-code-hooks-production-quality-ci-cd-patterns)
3. [Claude Code Permission Modes](https://code.claude.com/docs/en/permission-modes)
4. [Agent Skills - Claude API Docs](https://platform.claude.com/docs/en/agents-and-tools/agent-skills/overview)
5. [How to Use Agent Teams in Claude Code](https://www.techcompanynews.com/how-to-use-agent-teams-in-claude-code-to-have-multiple-claudes-work-on-your-project-at-once/)
6. [How Prompt Caching Actually Works in Claude Code](https://www.claudecodecamp.com/p/how-prompt-caching-actually-works-in-claude-code)
7. [Claude's Constitution](https://www.anthropic.com/news/claudes-constitution)
8. [GLM-4.6: Complete Guide, Pricing, Context Window](https://llm-stats.com/blog/research/glm-4-6-launch)
9. [CodeGeeX 2025 IDE Plugin Guide](https://www.9ku.com/djnews/bgwcw.html)
10. [AutoGLM Open Source](https://github.com/zai-org/Open-AutoGLM)
11. [Kimi K2: Moonshot AI's Agentic Powerhouse](https://aiadoptionagency.com/kimi-k2-moonshot-ais-agentic-powerhouse-for-code-and-complex-reasoning/)
12. [Codex CLI Approval Modes](https://allthings.how/codex-cli-skip-permissions-how-to-bypass-approvals-and-sandbox/)
13. [OpenAI Codex CLI Constraints](https://developers.openai.com/codex/agent-approvals-security)
14. [GPT-5.1 Prompting Guide](https://godofprompt.ai/blog/gpt-5-1-prompting-guide/)
15. [Cursor Rules: Advanced Pattern Configuration](https://www.sitepoint.com/cursor-rules-advanced-pattern-configuration-guide/)
16. [Windsurf Cascade: Guide and Best Practices](https://en.paradigmadigital.com/dev/windsurf-cascade-guide-best-practices/)
17. [Cline Rules: Autonomous Coding Agent Best Practices](https://cursor-alternatives.com/blog/cline-rules/)
18. [Continue.dev Documentation](https://docs.continue.dev)
19. [Aider Conventions](https://aider.chat/docs/usage/conventions.html)
20. [Google Jules AI Coding Agent - Complete Guide](https://chandanadev.com/google-jules-ai-coding-agent)
21. [Qwen Code Docs - Subagents](https://qwenlm.github.io/qwen-code-docs/en/users/features/sub-agents/)

### Academic Papers
22. [Constitutional AI: Harmlessness from AI Feedback](https://arxiv.org/abs/2212.08073)
23. [Self-Refine: Iterative Refinement with Self-Feedback](https://arxiv.org/abs/2303.17651)
24. [LLM Critics Help Catch LLM Bugs (CriticGPT)](https://arxiv.org/abs/2407.00215)
25. [Improving Factuality and Reasoning in Language Models through Multiagent Debate](https://arxiv.org/abs/2305.14325)
26. [MetaGPT: Meta Programming for A Multi-Agent Collaborative Framework](https://arxiv.org/abs/2308.00352)
27. [AgentCoder: Multi-Agent-based Code Generation](https://arxiv.org/abs/2312.13010)
28. [SWE-agent: Agent-Computer Interfaces Enable Automated Software Engineering](https://arxiv.org/abs/2405.15793)
29. [IRIS: LLM-Assisted Static Analysis for Detecting Security Vulnerabilities](https://arxiv.org/abs/2405.17238)
30. [Enhancing LLM-Generated Code Beyond Correctness](https://arxiv.org/abs/2508.14419)
31. [SafeCoder: Instruction Tuning for Secure Code Generation](https://arxiv.org/abs/2402.09497)
32. [Secure-Instruct: An Automated Pipeline for Synthesizing Instruction-Tuning Datasets](https://arxiv.org/abs/2510.07189)
33. [Detect-Repair-Verify for Securing LLM-Generated Code](https://arxiv.org/abs/2603.00897)
34. [CONCUR: Benchmarking LLMs for Concurrent Code Generation](https://arxiv.org/abs/2603.03683)
35. [How Efficient is LLM-Generated Code? (ENAMEL)](https://arxiv.org/abs/2406.06647)
36. [DR.FIX: Automatically Fixing Data Races at Industry Scale](https://arxiv.org/abs/2504.15637)
37. [CodeCRDT: Observation-Driven Coordination for Multi-Agent LLM Code Generation](https://arxiv.org/abs/2510.18893)
38. [Mergiraf: Syntax-Aware Merging for Git](https://mergiraf.org/)
39. [LLMergeJ / Merge-Bench](https://github.com/benedikt-schesch/Merge-Bench)
40. [The Fragility of LLM Agents in Backend Code Generation](https://arxiv.org/abs/2605.06445)
41. [CRANE: Reasoning with Constrained LLM Generation](https://arxiv.org/abs/2502.09061)
42. [Spaghetti Bench: Evaluating AI Agents on Concurrency](https://pastalab.org/spaghetti-bench/blog.html)
43. [C3AI: Crafting and Evaluating Constitutions for Constitutional AI](https://arxiv.org/abs/2502.15861)
44. [Evolving Interpretable Constitutions for Multi-Agent Coordination](https://arxiv.org/abs/2602.00755)
45. [LLM4TDD: Best Practices for Test Driven Development Using Large Language Models](https://arxiv.org/abs/2312.04687)
46. [Use Property-Based Testing to Bridge LLM Code Generation and Validation](https://arxiv.org/abs/2506.18315)
47. [StepCoder: Improve Code Generation with Reinforcement Learning from Compiler Feedback](https://arxiv.org/abs/2402.01391)
48. [OSS-Fuzz-Gen](https://github.com/google/oss-fuzz-gen)
49. [Voyager: An Open-Ended Embodied Agent with Large Language Models](https://arxiv.org/abs/2305.16291)
50. [DSPy: Compiling Declarative Language Model Calls into Self-Improving Pipelines](https://arxiv.org/abs/2310.03714)
