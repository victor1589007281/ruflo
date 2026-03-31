package guidance

import (
	"sync"
)

// ShardRetriever selects shards for an intent (pluggable).
type ShardRetriever interface {
	RetrieveForTask(intent TaskIntent, bundle PolicyBundle) []RuleShard
}

type simpleRetriever struct{}

func (simpleRetriever) RetrieveForTask(intent TaskIntent, bundle PolicyBundle) []RuleShard {
	var out []RuleShard
	for _, sh := range bundle.Shards {
		for _, in := range sh.Intents {
			if in == intent {
				out = append(out, sh)
				break
			}
		}
	}
	if len(out) == 0 {
		out = append(out, bundle.Shards...)
	}
	return out
}

// OptimizerHook is an optional compile-time optimizer callback.
type OptimizerHook interface {
	OnBundleCompiled(PolicyBundle) PolicyBundle
}

// GuidanceControlPlane wires compiler, retrieval, gates, ledger, and optimizer.
type GuidanceControlPlane struct {
	mu        sync.RWMutex
	compiler  *GuidanceCompiler
	retriever ShardRetriever
	gates     *EnforcementGates
	ledger    RunLedger
	optimizer OptimizerHook
	bundle    PolicyBundle
	rootMD    string
	localMD   string
	// AutoSave persists the bundle after Compile when AutoSavePath is set.
	AutoSave     bool
	AutoSavePath string
}

// NewGuidanceControlPlane constructs a control plane with defaults.
func NewGuidanceControlPlane() *GuidanceControlPlane {
	return &GuidanceControlPlane{
		compiler:  NewGuidanceCompiler(),
		retriever: simpleRetriever{},
		ledger:    NewStructuredRunLedger(),
	}
}

// SetDependencies replaces subcomponents.
func (p *GuidanceControlPlane) SetDependencies(retriever ShardRetriever, gates *EnforcementGates, ledger RunLedger, opt OptimizerHook) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if retriever != nil {
		p.retriever = retriever
	}
	if gates != nil {
		p.gates = gates
	}
	if ledger != nil {
		p.ledger = ledger
	}
	p.optimizer = opt
}

// SetSources stores markdown sources for Compile.
func (p *GuidanceControlPlane) SetSources(rootMD, localMD string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rootMD = rootMD
	p.localMD = localMD
}

// Compile builds or refreshes PolicyBundle from configured markdown.
func (p *GuidanceControlPlane) Compile() PolicyBundle {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.compiler.Compile(p.rootMD, p.localMD)
	if p.optimizer != nil {
		b = p.optimizer.OnBundleCompiled(b)
	}
	p.bundle = b
	p.gates = NewEnforcementGates(&b)
	if p.AutoSave && p.AutoSavePath != "" {
		_ = Save(b, p.AutoSavePath)
	}
	return b
}

// Bundle returns the last compiled bundle.
func (p *GuidanceControlPlane) Bundle() PolicyBundle {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.bundle
}

// RetrieveForTask returns shards for an intent.
func (p *GuidanceControlPlane) RetrieveForTask(intent TaskIntent) []RuleShard {
	p.mu.RLock()
	b := p.bundle
	r := p.retriever
	p.mu.RUnlock()
	if r == nil {
		return nil
	}
	return r.RetrieveForTask(intent, b)
}

// EvaluateCommand delegates to gates (compiles default bundle if needed).
func (p *GuidanceControlPlane) EvaluateCommand(cmd string) []GateResult {
	g := p.gatesInstance()
	return g.EvaluateCommand(cmd)
}

// EvaluateEdit delegates to gates.
func (p *GuidanceControlPlane) EvaluateEdit(file, diff string) []GateResult {
	g := p.gatesInstance()
	return g.EvaluateEdit(file, diff)
}

// EvaluateToolUse delegates to gates.
func (p *GuidanceControlPlane) EvaluateToolUse(tool string, args map[string]any) []GateResult {
	g := p.gatesInstance()
	return g.EvaluateToolUse(tool, args)
}

func (p *GuidanceControlPlane) gatesInstance() *EnforcementGates {
	p.mu.RLock()
	g := p.gates
	b := p.bundle
	p.mu.RUnlock()
	if g != nil {
		return g
	}
	if b.ID == "" {
		return NewEnforcementGates(nil)
	}
	return NewEnforcementGates(&b)
}

// StartRun records run start in the ledger.
func (p *GuidanceControlPlane) StartRun(runID string, meta map[string]any) {
	p.mu.RLock()
	l := p.ledger
	p.mu.RUnlock()
	if l != nil {
		l.StartRun(runID, meta)
	}
}

// FinalizeRun records completion.
func (p *GuidanceControlPlane) FinalizeRun(runID string, success bool, meta map[string]any) {
	p.mu.RLock()
	l := p.ledger
	p.mu.RUnlock()
	if l != nil {
		l.FinalizeRun(runID, success, meta)
	}
}
