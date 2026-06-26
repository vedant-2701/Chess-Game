package chess

// GameOutcome holds the result of a completed game. It maps to the store layer's
// Outcome and OutcomeReason types but is defined here to keep internal/chess free
// of internal/store dependencies (per the architecture dependency graph).
type GameOutcome struct {
	// Winner is "WHITE", "BLACK", or "DRAW". Matches the store.Outcome values.
	Winner string
	// Reason is "CHECKMATE", "STALEMATE", "TIMEOUT", "RESIGNATION", "ABANDONED",
	// or "DRAW_AGREEMENT". Matches store.OutcomeReason values.
	// For outcomes detected by this package, only CHECKMATE and STALEMATE are used.
	// RESIGNATION, TIMEOUT, and ABANDONED are set by the game layer, not the chess layer.
	Reason string
}