package store

import "time"

// StartingFEN is the standard chess starting position in Forsyth-Edwards Notation.
// Used as the default value for games.current_fen.
const StartingFEN = "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"

// GameStatus represents the lifecycle state of a game.
// Values must match the CHECK constraint in the games table migration.
type GameStatus string

const (
	GameStatusWaiting   GameStatus = "WAITING_FOR_PLAYER"
	GameStatusActive    GameStatus = "ACTIVE"
	GameStatusCompleted GameStatus = "COMPLETED"
	GameStatusAbandoned GameStatus = "ABANDONED"
	// GameStatusAborted (DECISIONS_LOG_PHASE_2.md ADR-029): the game's
	// creator connected but the opponent never joined before the
	// abandonment timer fired. Distinct from GameStatusAbandoned, which
	// requires the game to have actually reached ACTIVE (both players
	// joined) first — an ABORTED game never started, so it has no
	// meaningful winner and isn't scored as a draw either.
	GameStatusAborted GameStatus = "ABORTED"
)

// Color identifies which side a player or move belongs to.
// Values must match the CHECK constraint in the moves table migration.
type Color string

const (
	ColorWhite Color = "WHITE"
	ColorBlack Color = "BLACK"
)

// Outcome records who won a completed game, or that it was a draw.
// Values must match the CHECK constraint in the games table migration.
type Outcome string

const (
	OutcomeWhite Outcome = "WHITE"
	OutcomeBlack Outcome = "BLACK"
	OutcomeDraw  Outcome = "DRAW"
)

// OutcomeReason records why a game ended.
// Values must match the CHECK constraint in the games table migration.
type OutcomeReason string

const (
	OutcomeReasonCheckmate     OutcomeReason = "CHECKMATE"
	OutcomeReasonStalemate     OutcomeReason = "STALEMATE"
	OutcomeReasonResignation   OutcomeReason = "RESIGNATION"
	OutcomeReasonTimeout       OutcomeReason = "TIMEOUT"
	OutcomeReasonDrawAgreement OutcomeReason = "DRAW_AGREEMENT"
	OutcomeReasonAbandoned     OutcomeReason = "ABANDONED"
)

// GameOutcome pairs Outcome and OutcomeReason. They are always set together —
// it is not valid to record an outcome without a reason or vice versa.
type GameOutcome struct {
	Outcome Outcome
	Reason  OutcomeReason
}

// User represents a row in the users table.
// Identity is anonymous: userID is generated client-side and submitted on first game creation.
type User struct {
	ID        string
	CreatedAt time.Time
}

// Game represents a row in the games table.
// PlayerBlackID is nil until a second player joins.
// Outcome and OutcomeReason are nil until the game reaches a terminal state.
type Game struct {
	ID            string
	Status        GameStatus
	PlayerWhiteID string
	PlayerBlackID *string // nil while status is WAITING_FOR_PLAYER
	CurrentFEN    string
	WhiteTimeMs   int64
	BlackTimeMs   int64
	Outcome       *Outcome       // nil until game is over
	OutcomeReason *OutcomeReason // nil until game is over
	// WhiteDisconnectedAt / BlackDisconnectedAt (DECISIONS_LOG_PHASE_2.md
	// ADR-030): nil unless that color currently has a pending abandonment
	// grace period. Persisted so a surviving instance can resume (or
	// immediately resolve) the grace period after a failover —
	// Manager.abandonTimers is pure per-process memory and does not survive
	// the owning process dying mid-grace-period.
	WhiteDisconnectedAt *time.Time
	BlackDisconnectedAt *time.Time
	// MatchmakingRequestID (Phase 3, DECISIONS_LOG_PHASE_3.md ADR-034,
	// rationale corrected by ADR-039): nil for shared-link games
	// (CreateGame/UpdatePlayerBlack) — only matchmaking-originated games
	// populate it. One UUID v4 generated per ZPOPMIN-won pairing attempt on
	// the chess-server side and reused across every retry of that same
	// attempt; CreateMatchedGame's ON CONFLICT (matchmaking_request_id) DO
	// NOTHING is what makes retrying after an ambiguous failure
	// (timeout / lost ACK) safe rather than a double-booking risk.
	MatchmakingRequestID *string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Move represents a row in the moves table.
type Move struct {
	ID         int64
	GameID     string
	MoveNumber int
	Color      Color
	SAN        string // Standard Algebraic Notation: "e4", "Nf3", "O-O"
	FENAfter   string // Board state after this move applied
	PlayedAt   time.Time
}
