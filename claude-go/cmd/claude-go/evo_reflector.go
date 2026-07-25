package main

// evo_reflector.go —— 装配处的一层薄封装: prompt 反思器的档位选择。
//
// 实现在 pkg/agent.FallbackReflector (CLI 与飞书共用一份, 见那里的注释:
// §4.2 H3 禁止用被评估的主模型自评, 没 fallback 就返回 nil 而不是退而用主模型)。
// 这里只保留一个具名入口, 让 main.go 的装配行读起来是"给循环配个反思器"而不是
// 一串包路径。

import (
	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/evolution/learners"
)

// cliEvoReflector 见 agent.FallbackReflector。
func cliEvoReflector(c *api.Client) learners.Reflector { return agent.FallbackReflector(c) }
