package neural

import "time"

// SONAConfig tunes SONA learning dynamics.
type SONAConfig struct {
	LearningRate      float64 `json:"learning_rate"`
	LoraRank          int     `json:"lora_rank"`
	EWCLambda         float64 `json:"ewc_lambda"`
	MaxTrajectorySize int     `json:"max_trajectory_size"`
	MaxSignals        int     `json:"max_signals"`
	MaxPatterns       int     `json:"max_patterns"`
}

// DefaultSONAConfig returns sane defaults.
func DefaultSONAConfig() SONAConfig {
	return SONAConfig{
		LearningRate:      0.05,
		LoraRank:          8,
		EWCLambda:         0.4,
		MaxTrajectorySize: 256,
		MaxSignals:        512,
		MaxPatterns:       4096,
	}
}

// StepType classifies trajectory steps.
type StepType string

const (
	StepObservation StepType = "observation"
	StepThought     StepType = "thought"
	StepAction      StepType = "action"
	StepResult      StepType = "result"
)

// Pattern is a distilled neural pattern (SONA / ReasoningBank substrate).
type Pattern struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Embedding  []float32         `json:"embedding,omitempty"`
	Content    string            `json:"content"`
	Confidence float64           `json:"confidence"`
	UsageCount int               `json:"usage_count"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// Trajectory aggregates an episode.
type Trajectory struct {
	ID        string           `json:"id"`
	Steps     []TrajectoryStep `json:"steps"`
	Outcome   string           `json:"outcome,omitempty"`
	Reward    float64          `json:"reward"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// TrajectoryStep is one step in a trajectory.
type TrajectoryStep struct {
	Type      StepType          `json:"type"`
	Content   string            `json:"content"`
	Embedding []float32         `json:"embedding,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}

// Signal is a lightweight observation pushed to the circular buffer.
type Signal struct {
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}
