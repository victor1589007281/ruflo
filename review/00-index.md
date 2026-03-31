# Ruflo V3 源码 Review 总目录

> **审查日期**: 2026-03-31
> **审查范围**: Ruflo V3 (原 Claude Flow) 全部核心模块
> **版本**: v3.5.0 (5,900+ commits, 20 核心包, 259 MCP 工具, 60+ Agent 类型)

---

## 📄 文档索引

| # | 文件 | 内容 | 重点 |
|---|------|------|------|
| 01 | [架构总览](./01-architecture-overview.md) | 项目定位、整体架构图、包依赖关系、20 个核心包一览 | **Mermaid 架构图** |
| 02 | [模块深度解析](./02-module-deep-dive.md) | CLI 命令系统、MCP-First 设计、Memory/Hooks/Swarm/Security/Guidance/Providers/Codex 详解 | **内部实现原理** |
| 03 | [时序图详解](./03-sequence-diagrams.md) | 8 张完整时序图：MCP 调用、提示词注入、LLM 钩子、Swarm 编排、治理门控、SONA 学习、MCP 路由、模型路由 | **Mermaid 时序图** |
| 04 | [自动进化机制](./04-auto-evolution.md) | 4 层进化：运行时学习、SONA 自适应、治理进化、后台 Worker；跨会话持久化 | **进化机制** |
| 05 | [使用方式指南](./05-usage-guide.md) | 快速开始、Swarm 配方、内存操作、Hooks 使用、治理控制、安全/性能/插件 | **实操指南** |

---

## 🏗️ 核心架构一句话总结

```
用户 → Claude Code (Cursor) → MCP stdio 协议 → Ruflo TOOL_REGISTRY (259 工具)
                                                      ↓
                  ┌──────────────┬──────────────┬──────────────┐
                  │   Memory     │    Hooks     │    Swarm     │
                  │  HNSW 搜索   │  ReasoningBank│  15-Agent    │
                  │  AgentDB     │  12 Workers   │  共识算法     │
                  └──────────────┴──────────────┴──────────────┘
                  ┌──────────────┬──────────────┬──────────────┐
                  │  Guidance    │  Providers   │   Neural     │
                  │  治理门控     │  6+ LLM 适配  │  SONA/EWC++  │
                  └──────────────┴──────────────┴──────────────┘
```

---

## 🔑 关键设计决策

| ADR | 决策 | 影响 |
|-----|------|------|
| **ADR-003** | 统一 Swarm 协调器 | 合并 4 个遗留系统为 `UnifiedSwarmCoordinator` |
| **ADR-005** | MCP-First 设计 | CLI 是 MCP 工具的薄包装，所有逻辑在 tool handler |
| **ADR-006** | 统一内存服务 | AgentDB + HNSW 替代多种内存后端 |
| **ADR-009** | 混合内存后端 | SQLite (结构化) + AgentDB (语义) 双写 |
| **ADR-026** | 3 层模型路由 | Booster (<1ms/$0) → Haiku (~500ms) → Sonnet/Opus |

---

## 📊 性能指标

| 指标 | 目标 | 状态 |
|------|------|------|
| HNSW 搜索 | 150x-12,500x 加速 | ✅ 已实现 |
| 内存缩减 | 50-75% (Int8 量化) | ✅ 已实现 (3.92x) |
| MCP 响应 | <100ms | ✅ 已达成 |
| CLI 启动 | <500ms | ✅ 已达成 |
| SONA 适应 | <0.05ms | 🔄 进行中 |
| Flash Attention | 2.49x-7.47x | 🔄 进行中 |

---

## 🧬 自进化核心循环

```
RETRIEVE (HNSW 检索) → JUDGE (判定奖励) → DISTILL (LoRA 蒸馏) → CONSOLIDATE (EWC++ 防遗忘)
     ↑                                                                    ↓
     └──────────────────── 下次任务自动受益 ←─────────────────────────────┘
```
