package swarm

import "github.com/ruflo/ruflo-go/api"

func agentHealth(a *api.Agent) float64 {
	if a == nil || a.Metrics.Extra == nil {
		return 1
	}
	h, ok := a.Metrics.Extra["health"]
	if !ok {
		return 1
	}
	return h
}

func setAgentHealth(a *api.Agent, h float64) {
	if a.Metrics.Extra == nil {
		a.Metrics.Extra = make(map[string]float64)
	}
	a.Metrics.Extra["health"] = h
}

func agentLoad(a *api.Agent) float64 {
	if a == nil || a.Metrics.Extra == nil {
		return 0
	}
	return a.Metrics.Extra["load"]
}

func setAgentLoad(a *api.Agent, v float64) {
	if a.Metrics.Extra == nil {
		a.Metrics.Extra = make(map[string]float64)
	}
	a.Metrics.Extra["load"] = v
}

func agentCapabilities(a *api.Agent) []string {
	if a == nil {
		return nil
	}
	var out []string
	out = append(out, a.Capabilities.Skills...)
	out = append(out, a.Capabilities.Tools...)
	return out
}
