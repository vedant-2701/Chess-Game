package game

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewRedisClient constructs and connects a Redis client for Phase 2's routing
// directory (ownership + liveness keys — see phases/current/PHASE_2.md and
// DECISIONS_LOG_PHASE_2.md ADR-021/ADR-023).
//
// This file currently holds only the client constructor — PHASE_2.md's
// Implementation Checklist Step 1. The RoutingDirectory interface and
// RedisDirectory implementation (ClaimOwnership, GetOwner, RenewOwnership,
// ReleaseOwnership, SetAlive, IsAlive, RenewAlive) are added in Step 2 and are
// deliberately not present yet — mirrors internal/store/postgres.go's
// NewPool, which constructs and verifies the raw connection only; GameStore
// and MoveStore (the domain-shaped wrappers) are separate.
//
// redisAddr is a host:port pair (e.g. "localhost:6379"), matching
// docker-compose.yml's redis service — not a redis:// URL. Connectivity is
// verified with a single bounded PING before returning: a failure here is
// fatal at startup, the same treatment store.NewPool gives a bad
// DATABASE_URL. There is no meaningful degraded startup mode for an instance
// that cannot reach its routing directory at all during boot.
//
// This is distinct from a *later*, in-flight Redis outage after the server
// is already running. PHASE_2.md's Step 1 acceptance requirement — an
// already-owned, already-hydrated game must keep working even if Redis
// becomes briefly unavailable after startup, with only *new* resolve calls
// failing — is a property of how Step 2+'s RoutingDirectory call sites
// handle errors during normal operation, not of this constructor, which only
// governs the one-time initial connectivity check.
func NewRedisClient(ctx context.Context, redisAddr string) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := client.Ping(pingCtx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("game.NewRedisClient addr=%s: %w", redisAddr, err)
	}

	return client, nil
}

// --- PHASE_2.md Step 2: Routing Directory -----------------------------------

const (
	// OwnershipTTL is the expiry on the game:{gameID} ownership record.
	// Deliberately longer than LivenessTTL and NOT itself the failure-
	// detection signal — see DECISIONS_LOG_PHASE_2.md ADR-023's two-key
	// rationale. Ownership is meant to be stable; liveness is meant to be
	// fast.
	OwnershipTTL = 30 * time.Second

	// OwnershipRenewInterval is how often the Step 6 heartbeat ticker must
	// renew ownership for every locally-active game to stay comfortably
	// inside OwnershipTTL. Defined here for documentation/reference only —
	// this file only knows about TTLs, not renewal cadence; the ticker itself
	// (Step 6, not yet implemented) owns that.
	OwnershipRenewInterval = 10 * time.Second

	// LivenessTTL is the expiry on instance_alive:{instanceID} — the actual
	// failure-detection signal (ADR-023). Deliberately tighter than
	// OwnershipTTL so a dead instance is detected well before its stale
	// ownership records would otherwise expire on their own.
	LivenessTTL = 10 * time.Second

	// LivenessRenewInterval is how often the heartbeat ticker renews this
	// instance's own liveness key. Reference only, as above.
	LivenessRenewInterval = 3 * time.Second
)

// RoutingDirectory answers the two questions PHASE_2.md's resolve-then-connect
// flow needs answered before minting a ConnectClaims: "which instance
// currently owns this game" and "is that instance still alive." It knows
// nothing about GameSession, HTTP, or WebSockets — that orchestration belongs
// to the Step 5 resolve handler, not here.
//
// Ownership and liveness are deliberately separate concerns (ADR-023), not
// bundled into one call each: a caller typically calls GetOwner, then
// IsAlive on the result, as two separate decisions, matching the two-key
// design rather than hiding it behind a single combined method.
type RoutingDirectory interface {
	// ClaimOwnership atomically sets gameID's owner to instanceID.
	//
	// expectedPriorOwner encodes the precondition under which the claim may
	// succeed:
	//   - "" (empty string): claim succeeds only if no owner is currently
	//     recorded — the first-ever claim for this game, which is also the
	//     ordinary "round-robin lands two independent resolves on two
	//     instances" race described in PHASE_2.md's Connection Flow.
	//   - non-empty: claim succeeds only if the currently recorded owner
	//     exactly equals expectedPriorOwner — a takeover from an instance the
	//     caller has already confirmed dead via IsAlive (PHASE_2.md Connection
	//     Flow step 3's "Miss, expired, or instance_alive:{owner} absent"
	//     branch).
	//
	// Both cases are the same atomic compare-and-swap primitive, evaluated as
	// a single Redis operation, so two callers racing the identical
	// transition — two instances both deciding the same dead owner should be
	// replaced, or two instances both racing a brand-new game's first claim —
	// cannot both win. This mirrors the same discipline
	// DECISIONS_LOG_PHASE_1.md ADR-016 and this session's UpdateGameStatus fix
	// already established for Postgres: an atomic conditional write at the
	// point of contention, not a read-then-write check in application code.
	//
	// Returns claimed=false, err=nil when the precondition simply didn't hold
	// — an ordinary, expected race loss, not a failure. Callers should follow
	// a false result with GetOwner to discover who actually won.
	ClaimOwnership(ctx context.Context, gameID, instanceID, expectedPriorOwner string) (claimed bool, err error)

	// GetOwner returns the instanceID currently recorded as owning gameID. ok
	// is false if no ownership record exists (never claimed, or expired).
	GetOwner(ctx context.Context, gameID string) (instanceID string, ok bool, err error)

	// RenewOwnership extends gameID's ownership TTL, but only if instanceID
	// still matches the recorded owner — a compare-and-swap, not a blind
	// EXPIRE, so an instance that has already lost a game via a legitimate
	// takeover cannot resurrect its own stale claim through its own heartbeat
	// ticker. Returns renewed=false, err=nil if the instance no longer owns
	// the game — an expected outcome after a takeover, not itself an error.
	RenewOwnership(ctx context.Context, gameID, instanceID string) (renewed bool, err error)

	// ReleaseOwnership deletes gameID's ownership record, but only if
	// instanceID still matches the recorded owner — the same compare-and-swap
	// discipline as RenewOwnership, so a delayed or retried release call
	// issued after ownership has already legitimately moved elsewhere cannot
	// delete the new owner's record. Used during graceful shutdown
	// (PHASE_2.md Scope) to proactively release owned entries rather than
	// making a deliberate scale-down wait out OwnershipTTL.
	ReleaseOwnership(ctx context.Context, gameID, instanceID string) error

	// SetAlive records that instanceID is up, with LivenessTTL. Called once
	// at startup, before the heartbeat ticker's periodic RenewAlive takes
	// over. Unlike ownership, there is no compare-and-swap concern here: an
	// instance only ever writes its own liveness key, under its own
	// instanceID — there is no other legitimate writer to race against.
	SetAlive(ctx context.Context, instanceID string) error

	// IsAlive reports whether instanceID's liveness key is currently present.
	// A false result means either the instance never called SetAlive, or its
	// heartbeat ticker has stopped renewing for at least LivenessTTL — the
	// actual failure-detection signal (ADR-023).
	IsAlive(ctx context.Context, instanceID string) (bool, error)

	// RenewAlive extends instanceID's liveness TTL. Same underlying operation
	// as SetAlive (a plain SET with expiry, not a compare-and-swap — see
	// SetAlive's doc comment) — kept as a distinct method to match the
	// separate initial-set-at-startup vs. periodic-renew-on-a-ticker call
	// sites PHASE_2.md's heartbeat ticker (Step 6) will have.
	RenewAlive(ctx context.Context, instanceID string) error

	// RenewOwnershipBatch renews the ownership TTL for every gameID in one
	// round-trip, applying the same compare-and-swap discipline as
	// RenewOwnership per key (only renewed if instanceID still matches that
	// key's recorded owner). This is PHASE_2.md Step 6's heartbeat ticker's
	// actual call shape: DECISIONS_LOG_PHASE_2.md ADR-023's Consequences
	// section is explicit that the ticker performs "two Redis writes per
	// tick (batched ownership renewal across all locally-active games, plus
	// one liveness renewal), not one" — an instance hosting N games must not
	// cost N+1 Redis round-trips per tick.
	//
	// Returns a map from gameID to whether that specific key was renewed.
	// Callers should treat renewed=false for any gameID as "this instance no
	// longer legitimately owns that game" — the same expected, non-error
	// outcome RenewOwnership documents, now surfaced per-key across a batch.
	// An empty gameIDs slice is a valid no-op, not an error.
	RenewOwnershipBatch(ctx context.Context, gameIDs []string, instanceID string) (renewed map[string]bool, err error)

	// ReleaseAlive deletes instanceID's liveness key immediately, rather than
	// waiting for LivenessTTL to lapse naturally. Used during graceful
	// shutdown (PHASE_2.md Scope), alongside ReleaseOwnership, so a
	// deliberate scale-down doesn't leave a several-second window where other
	// instances still believe this one is alive. No compare-and-swap concern
	// — same reasoning as SetAlive/RenewAlive.
	ReleaseAlive(ctx context.Context, instanceID string) error
}

// var _ RoutingDirectory = (*RedisDirectory)(nil) — compile-time interface
// check, CODING_GUIDELINES.md §5.
var _ RoutingDirectory = (*RedisDirectory)(nil)

// RedisDirectory is the Redis-backed RoutingDirectory implementation.
type RedisDirectory struct {
	client *redis.Client
}

// NewRedisDirectory constructs a RedisDirectory over an already-connected,
// already-verified client (see NewRedisClient). Mirrors how GameStore and
// MoveStore wrap an already-verified *pgxpool.Pool (internal/store) rather
// than managing their own connection lifecycle.
func NewRedisDirectory(client *redis.Client) *RedisDirectory {
	return &RedisDirectory{client: client}
}

func ownershipKey(gameID string) string {
	return "game:" + gameID
}

func livenessKey(instanceID string) string {
	return "instance_alive:" + instanceID
}

// claimOwnershipScript is the compare-and-swap primitive behind
// ClaimOwnership: set the key to the new owner only if its current value
// equals expectedPriorOwner ("" meaning "must not currently exist"). A single
// Lua script keeps the read-compare-write atomic against every other client
// talking to this Redis instance.
//
// Redis represents a missing key's GET result as Lua boolean false, not "" —
// the script special-cases that against ARGV[1] == "" explicitly, rather than
// comparing false == "" (which is never true in Lua).
//
// KEYS[1] = ownership key
// ARGV[1] = expectedPriorOwner ("" for "must not exist")
// ARGV[2] = new owner (instanceID)
// ARGV[3] = TTL in seconds
var claimOwnershipScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if (cur == false and ARGV[1] == "") or (cur ~= false and cur == ARGV[1]) then
	redis.call("SET", KEYS[1], ARGV[2], "EX", ARGV[3])
	return 1
else
	return 0
end
`)

func (d *RedisDirectory) ClaimOwnership(ctx context.Context, gameID, instanceID, expectedPriorOwner string) (bool, error) {
	res, err := claimOwnershipScript.Run(ctx, d.client, []string{ownershipKey(gameID)},
		expectedPriorOwner, instanceID, int64(OwnershipTTL.Seconds())).Result()
	if err != nil {
		return false, fmt.Errorf("RedisDirectory.ClaimOwnership gameID=%s instanceID=%s: %w", gameID, instanceID, err)
	}
	claimed, ok := res.(int64)
	if !ok {
		return false, fmt.Errorf("RedisDirectory.ClaimOwnership gameID=%s instanceID=%s: unexpected script result type %T", gameID, instanceID, res)
	}
	return claimed == 1, nil
}

func (d *RedisDirectory) GetOwner(ctx context.Context, gameID string) (string, bool, error) {
	val, err := d.client.Get(ctx, ownershipKey(gameID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("RedisDirectory.GetOwner gameID=%s: %w", gameID, err)
	}
	return val, true, nil
}

// renewOwnershipScript extends the ownership key's TTL only if its current
// value still equals instanceID — the same compare-and-swap discipline as
// claimOwnershipScript, preventing an instance from resurrecting a claim it
// has already lost via its own periodic renewal call.
//
// KEYS[1] = ownership key
// ARGV[1] = instanceID (expected current owner)
// ARGV[2] = TTL in seconds
var renewOwnershipScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if cur ~= false and cur == ARGV[1] then
	redis.call("EXPIRE", KEYS[1], ARGV[2])
	return 1
else
	return 0
end
`)

func (d *RedisDirectory) RenewOwnership(ctx context.Context, gameID, instanceID string) (bool, error) {
	res, err := renewOwnershipScript.Run(ctx, d.client, []string{ownershipKey(gameID)},
		instanceID, int64(OwnershipTTL.Seconds())).Result()
	if err != nil {
		return false, fmt.Errorf("RedisDirectory.RenewOwnership gameID=%s instanceID=%s: %w", gameID, instanceID, err)
	}
	renewed, ok := res.(int64)
	if !ok {
		return false, fmt.Errorf("RedisDirectory.RenewOwnership gameID=%s instanceID=%s: unexpected script result type %T", gameID, instanceID, res)
	}
	return renewed == 1, nil
}

// releaseOwnershipScript deletes the ownership key only if its current value
// still equals instanceID — the same compare-and-swap discipline as
// renewOwnershipScript, so a release call cannot delete a different
// instance's legitimately-newer claim.
//
// KEYS[1] = ownership key
// ARGV[1] = instanceID (expected current owner)
var releaseOwnershipScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if cur ~= false and cur == ARGV[1] then
	redis.call("DEL", KEYS[1])
end
return 1
`)

func (d *RedisDirectory) ReleaseOwnership(ctx context.Context, gameID, instanceID string) error {
	if _, err := releaseOwnershipScript.Run(ctx, d.client, []string{ownershipKey(gameID)}, instanceID).Result(); err != nil {
		return fmt.Errorf("RedisDirectory.ReleaseOwnership gameID=%s instanceID=%s: %w", gameID, instanceID, err)
	}
	return nil
}

func (d *RedisDirectory) SetAlive(ctx context.Context, instanceID string) error {
	if err := d.client.Set(ctx, livenessKey(instanceID), instanceID, LivenessTTL).Err(); err != nil {
		return fmt.Errorf("RedisDirectory.SetAlive instanceID=%s: %w", instanceID, err)
	}
	return nil
}

func (d *RedisDirectory) IsAlive(ctx context.Context, instanceID string) (bool, error) {
	n, err := d.client.Exists(ctx, livenessKey(instanceID)).Result()
	if err != nil {
		return false, fmt.Errorf("RedisDirectory.IsAlive instanceID=%s: %w", instanceID, err)
	}
	return n > 0, nil
}

func (d *RedisDirectory) RenewAlive(ctx context.Context, instanceID string) error {
	if err := d.client.Set(ctx, livenessKey(instanceID), instanceID, LivenessTTL).Err(); err != nil {
		return fmt.Errorf("RedisDirectory.RenewAlive instanceID=%s: %w", instanceID, err)
	}
	return nil
}

// renewOwnershipBatchScript is renewOwnershipScript generalized to N keys in
// one round-trip — KEYS[i] is renewed (compare-and-swap against ARGV[1])
// independently, with per-key results returned as a Lua table (translated by
// go-redis into a []interface{} of int64s, position-matched to KEYS).
//
// KEYS = ownership keys to renew
// ARGV[1] = instanceID (expected current owner for every key)
// ARGV[2] = TTL in seconds
var renewOwnershipBatchScript = redis.NewScript(`
local results = {}
for i, key in ipairs(KEYS) do
	local cur = redis.call("GET", key)
	if cur ~= false and cur == ARGV[1] then
		redis.call("EXPIRE", key, ARGV[2])
		results[i] = 1
	else
		results[i] = 0
	end
end
return results
`)

func (d *RedisDirectory) RenewOwnershipBatch(ctx context.Context, gameIDs []string, instanceID string) (map[string]bool, error) {
	result := make(map[string]bool, len(gameIDs))
	if len(gameIDs) == 0 {
		return result, nil
	}

	keys := make([]string, len(gameIDs))
	for i, gameID := range gameIDs {
		keys[i] = ownershipKey(gameID)
	}

	res, err := renewOwnershipBatchScript.Run(ctx, d.client, keys, instanceID, int64(OwnershipTTL.Seconds())).Result()
	if err != nil {
		return nil, fmt.Errorf("RedisDirectory.RenewOwnershipBatch instanceID=%s count=%d: %w", instanceID, len(gameIDs), err)
	}

	values, ok := res.([]interface{})
	if !ok || len(values) != len(gameIDs) {
		return nil, fmt.Errorf("RedisDirectory.RenewOwnershipBatch instanceID=%s: unexpected script result shape %T (len %d, want %d)",
			instanceID, res, len(values), len(gameIDs))
	}

	for i, gameID := range gameIDs {
		v, ok := values[i].(int64)
		if !ok {
			return nil, fmt.Errorf("RedisDirectory.RenewOwnershipBatch instanceID=%s gameID=%s: unexpected element type %T", instanceID, gameID, values[i])
		}
		result[gameID] = v == 1
	}
	return result, nil
}

func (d *RedisDirectory) ReleaseAlive(ctx context.Context, instanceID string) error {
	if err := d.client.Del(ctx, livenessKey(instanceID)).Err(); err != nil {
		return fmt.Errorf("RedisDirectory.ReleaseAlive instanceID=%s: %w", instanceID, err)
	}
	return nil
}
