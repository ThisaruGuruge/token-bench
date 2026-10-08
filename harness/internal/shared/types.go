// Package shared contains data types shared by harness implementations.
package shared

// TokenCount contains token usage dimensions reported by a coding harness.
type TokenCount struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Total      int `json:"total"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

// InfraError is a failure of the harness process or its model API (a crash,
// an API error that ended the turn), not of the agent's work. A run that ends
// on one says nothing about the agent and is rerun.
type InfraError struct{ Err error }

func (e *InfraError) Error() string { return e.Err.Error() }
func (e *InfraError) Unwrap() error { return e.Err }

// StopError is a session the harness stopped on a limit, such as the spend cap
// or the turn limit. It is an agent outcome, recorded with its reason.
type StopError struct {
	Reason string
	Detail string
}

func (e *StopError) Error() string {
	if e.Detail == "" {
		return "agent stopped: " + e.Reason
	}
	return "agent stopped: " + e.Reason + ": " + e.Detail
}
