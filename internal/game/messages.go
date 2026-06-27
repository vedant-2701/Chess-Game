package game

// WebSocket message types. Used in both EventBus event types and in the
// "type" field of every WebSocket JSON message. These are the single
// source of truth — never hardcode these strings elsewhere in the codebase.

// Client → Server message types.
const (
	MsgTypeMove   = "MOVE"
	MsgTypeResign = "RESIGN"
	MsgTypePing   = "PING"
)

// Server → Client message types.
const (
	MsgTypePong                 = "PONG"
	MsgTypeGameState            = "GAME_STATE"
	MsgTypeMoveApplied          = "MOVE_APPLIED"
	MsgTypeMoveRejected         = "MOVE_REJECTED"
	MsgTypeGameOver             = "GAME_OVER"
	MsgTypeOpponentConnected    = "OPPONENT_CONNECTED"
	MsgTypeOpponentDisconnected = "OPPONENT_DISCONNECTED"
	MsgTypeOpponentReconnected  = "OPPONENT_RECONNECTED"
	MsgTypeError                = "ERROR"
)

// MOVE_REJECTED reasons. Sent in the "reason" field of MOVE_REJECTED messages.
const (
	RejectReasonNotYourTurn   = "not your turn"
	RejectReasonIllegalMove   = "illegal move"
	RejectReasonGameNotActive = "game not active"
)

// ERROR codes. Sent in the "code" field of ERROR messages.
const (
	ErrCodeInvalidToken  = "INVALID_TOKEN"
	ErrCodeGameNotFound  = "GAME_NOT_FOUND"
	ErrCodeGameFull      = "GAME_FULL"
	ErrCodeInternalError = "INTERNAL_ERROR"
)