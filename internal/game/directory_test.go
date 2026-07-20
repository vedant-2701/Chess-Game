//go:build integration

package game

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func newTestDirectory() *RedisDirectory {
	return NewRedisDirectory(testRedisClient)
}

// --- ClaimOwnership -----------------------------------------------------

func TestRedisDirectory_ClaimOwnership_FreshClaim(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	claimed, err := d.ClaimOwnership(ctx, gameID, "instance-a", "")
	if err != nil {
		t.Fatalf("ClaimOwnership: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim to succeed for a never-before-claimed game")
	}

	owner, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok {
		t.Fatal("expected ownership record to exist after claim")
	}
	if owner != "instance-a" {
		t.Errorf("owner: got %q, want %q", owner, "instance-a")
	}
}

func TestRedisDirectory_ClaimOwnership_FreshClaim_AlreadyOwned(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	if _, err := d.ClaimOwnership(ctx, gameID, "instance-a", ""); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// A second instance attempting a *fresh* claim (expectedPriorOwner="")
	// against a game that already has a legitimate owner must lose — this is
	// the ordinary "round-robin lands two resolves on two instances" case
	// PHASE_2.md's Connection Flow describes, not the dead-owner-takeover
	// case.
	claimed, err := d.ClaimOwnership(ctx, gameID, "instance-b", "")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("expected second fresh claim to fail — game is already owned")
	}

	owner, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok || owner != "instance-a" {
		t.Errorf("expected owner to remain instance-a, got owner=%q ok=%v", owner, ok)
	}
}

func TestRedisDirectory_ClaimOwnership_Takeover_Succeeds(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	if _, err := d.ClaimOwnership(ctx, gameID, "instance-a", ""); err != nil {
		t.Fatalf("initial claim: %v", err)
	}

	// instance-b has independently confirmed (via GetOwner + IsAlive, not
	// exercised here — that's the resolve handler's job in Step 5) that
	// instance-a is dead, and takes over.
	claimed, err := d.ClaimOwnership(ctx, gameID, "instance-b", "instance-a")
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected takeover to succeed when expectedPriorOwner matches the actual current owner")
	}

	owner, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok || owner != "instance-b" {
		t.Errorf("expected owner=instance-b, got owner=%q ok=%v", owner, ok)
	}
}

func TestRedisDirectory_ClaimOwnership_Takeover_WrongExpectedOwner_Fails(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	if _, err := d.ClaimOwnership(ctx, gameID, "instance-a", ""); err != nil {
		t.Fatalf("initial claim: %v", err)
	}

	// instance-c believes instance-x owned the game (stale/wrong belief) —
	// the actual owner is instance-a, so this must fail and leave instance-a
	// untouched, not silently steal ownership based on a mistaken guess.
	claimed, err := d.ClaimOwnership(ctx, gameID, "instance-c", "instance-x")
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if claimed {
		t.Fatal("expected takeover to fail when expectedPriorOwner does not match the actual current owner")
	}

	owner, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok || owner != "instance-a" {
		t.Errorf("expected owner to remain instance-a, got owner=%q ok=%v", owner, ok)
	}
}

func TestRedisDirectory_ClaimOwnership_SetsExpiry(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	if _, err := d.ClaimOwnership(ctx, gameID, "instance-a", ""); err != nil {
		t.Fatalf("ClaimOwnership: %v", err)
	}

	ttl, err := testRedisClient.TTL(ctx, ownershipKey(gameID)).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > OwnershipTTL {
		t.Errorf("expected TTL in (0, %s], got %s", OwnershipTTL, ttl)
	}
}

// --- GetOwner -------------------------------------------------------------

func TestRedisDirectory_GetOwner_NeverClaimed(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()

	owner, ok, err := d.GetOwner(ctx, uuid.NewString())
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false for a never-claimed game, got owner=%q", owner)
	}
}

// --- RenewOwnership ---------------------------------------------------------

func TestRedisDirectory_RenewOwnership_Success(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	if _, err := d.ClaimOwnership(ctx, gameID, "instance-a", ""); err != nil {
		t.Fatalf("ClaimOwnership: %v", err)
	}

	renewed, err := d.RenewOwnership(ctx, gameID, "instance-a")
	if err != nil {
		t.Fatalf("RenewOwnership: %v", err)
	}
	if !renewed {
		t.Fatal("expected renewal to succeed for the actual current owner")
	}
}

func TestRedisDirectory_RenewOwnership_WrongInstance_Fails(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	if _, err := d.ClaimOwnership(ctx, gameID, "instance-a", ""); err != nil {
		t.Fatalf("ClaimOwnership: %v", err)
	}

	// instance-b (never owned this game) attempts to renew — must fail, not
	// silently extend a claim it doesn't hold.
	renewed, err := d.RenewOwnership(ctx, gameID, "instance-b")
	if err != nil {
		t.Fatalf("RenewOwnership: %v", err)
	}
	if renewed {
		t.Fatal("expected renewal to fail for an instance that does not own the game")
	}

	owner, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok || owner != "instance-a" {
		t.Errorf("expected owner to remain instance-a, got owner=%q ok=%v", owner, ok)
	}
}

func TestRedisDirectory_RenewOwnership_NonexistentGame_Fails(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()

	renewed, err := d.RenewOwnership(ctx, uuid.NewString(), "instance-a")
	if err != nil {
		t.Fatalf("RenewOwnership: %v", err)
	}
	if renewed {
		t.Fatal("expected renewal to fail for a game with no ownership record at all")
	}
}

// --- ReleaseOwnership --------------------------------------------------

func TestRedisDirectory_ReleaseOwnership_Success(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	if _, err := d.ClaimOwnership(ctx, gameID, "instance-a", ""); err != nil {
		t.Fatalf("ClaimOwnership: %v", err)
	}
	if err := d.ReleaseOwnership(ctx, gameID, "instance-a"); err != nil {
		t.Fatalf("ReleaseOwnership: %v", err)
	}

	_, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if ok {
		t.Fatal("expected ownership record to be gone after release")
	}
}

func TestRedisDirectory_ReleaseOwnership_WrongInstance_DoesNotDelete(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	if _, err := d.ClaimOwnership(ctx, gameID, "instance-a", ""); err != nil {
		t.Fatalf("ClaimOwnership: %v", err)
	}

	// A delayed/retried release from an instance that no longer owns the
	// game (e.g. its own shutdown-release call arriving late, after a
	// legitimate takeover already happened) must not delete the new owner's
	// record.
	if err := d.ReleaseOwnership(ctx, gameID, "instance-b"); err != nil {
		t.Fatalf("ReleaseOwnership: %v", err)
	}

	owner, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok || owner != "instance-a" {
		t.Errorf("expected owner to remain instance-a, got owner=%q ok=%v", owner, ok)
	}
}

// --- Liveness: SetAlive / IsAlive / RenewAlive ------------------------------

func TestRedisDirectory_SetAlive_ThenIsAlive(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()

	if err := d.SetAlive(ctx, "instance-a"); err != nil {
		t.Fatalf("SetAlive: %v", err)
	}

	alive, err := d.IsAlive(ctx, "instance-a")
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if !alive {
		t.Fatal("expected instance-a to be alive after SetAlive")
	}
}

func TestRedisDirectory_IsAlive_NeverSet(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()

	alive, err := d.IsAlive(ctx, "instance-never-started")
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if alive {
		t.Fatal("expected an instance that never called SetAlive to be reported not alive")
	}
}

func TestRedisDirectory_SetAlive_SetsExpiry(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()

	if err := d.SetAlive(ctx, "instance-a"); err != nil {
		t.Fatalf("SetAlive: %v", err)
	}

	ttl, err := testRedisClient.TTL(ctx, livenessKey("instance-a")).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > LivenessTTL {
		t.Errorf("expected TTL in (0, %s], got %s", LivenessTTL, ttl)
	}
}

func TestRedisDirectory_RenewAlive_ExtendsExpiry(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()

	if err := d.SetAlive(ctx, "instance-a"); err != nil {
		t.Fatalf("SetAlive: %v", err)
	}
	// Deliberately not sleeping to "watch it expire" (CODING_GUIDELINES.md
	// §8 forbids time.Sleep as test synchronization) — RenewAlive's contract
	// is simply "resets TTL to LivenessTTL," verified directly via TTL,
	// which is what matters, not the passage of wall-clock time.
	if err := d.RenewAlive(ctx, "instance-a"); err != nil {
		t.Fatalf("RenewAlive: %v", err)
	}

	alive, err := d.IsAlive(ctx, "instance-a")
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if !alive {
		t.Fatal("expected instance-a to still be alive after RenewAlive")
	}

	ttl, err := testRedisClient.TTL(ctx, livenessKey("instance-a")).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > LivenessTTL {
		t.Errorf("expected TTL in (0, %s], got %s", LivenessTTL, ttl)
	}
}

// --- Concurrent claim race: PHASE_2.md Step 2's explicit requirement -------

// TestRedisDirectory_ConcurrentFreshClaim_ExactlyOneWinner reproduces the
// ordinary Phase 2 race PHASE_2.md's Connection Flow describes: two
// instances' independent resolve calls for the SAME never-before-claimed
// game arrive concurrently, both see no owner, and both attempt a fresh
// claim. Exactly one must win — this is the split-brain bug ADR-021 exists
// to prevent, at the lowest possible level (the directory primitive itself),
// not just at the higher-level resolve-handler orchestration (Step 5, not
// yet implemented).
func TestRedisDirectory_ConcurrentFreshClaim_ExactlyOneWinner(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	const n = 10
	instanceIDs := make([]string, n)
	for i := range instanceIDs {
		instanceIDs[i] = uuid.NewString()
	}

	type result struct {
		instanceID string
		claimed    bool
	}
	results := make(chan result, n)

	for _, id := range instanceIDs {
		go func(instanceID string) {
			claimed, err := d.ClaimOwnership(ctx, gameID, instanceID, "")
			if err != nil {
				t.Errorf("ClaimOwnership instanceID=%s: %v", instanceID, err)
				results <- result{instanceID: instanceID, claimed: false}
				return
			}
			results <- result{instanceID: instanceID, claimed: claimed}
		}(id)
	}

	winners := 0
	var winnerID string
	for i := 0; i < n; i++ {
		r := <-results
		if r.claimed {
			winners++
			winnerID = r.instanceID
		}
	}

	if winners != 1 {
		t.Fatalf("expected exactly 1 winner among %d concurrent fresh claims, got %d", n, winners)
	}

	owner, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok {
		t.Fatal("expected an ownership record to exist after the race")
	}
	if owner != winnerID {
		t.Errorf("persisted owner %q does not match the reported winner %q", owner, winnerID)
	}
}

// TestRedisDirectory_ConcurrentTakeover_ExactlyOneWinner reproduces the more
// subtle race this design deliberately closes (see DECISIONS_LOG_PHASE_2.md
// ADR-023's discussion of TD-P2-001's residual window): multiple instances
// each independently concluding the SAME dead owner should be replaced, and
// racing to take over. The compare-and-swap must ensure exactly one takeover
// succeeds — a second instance's takeover attempt, evaluated after the first
// has already changed the stored value away from the expected dead owner,
// must fail its precondition and lose cleanly, not overwrite the winner.
func TestRedisDirectory_ConcurrentTakeover_ExactlyOneWinner(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	deadOwner := "instance-dead"
	if _, err := d.ClaimOwnership(ctx, gameID, deadOwner, ""); err != nil {
		t.Fatalf("initial claim by soon-to-be-dead owner: %v", err)
	}

	const n = 10
	challengerIDs := make([]string, n)
	for i := range challengerIDs {
		challengerIDs[i] = uuid.NewString()
	}

	type result struct {
		instanceID string
		claimed    bool
	}
	results := make(chan result, n)

	for _, id := range challengerIDs {
		go func(instanceID string) {
			claimed, err := d.ClaimOwnership(ctx, gameID, instanceID, deadOwner)
			if err != nil {
				t.Errorf("ClaimOwnership instanceID=%s: %v", instanceID, err)
				results <- result{instanceID: instanceID, claimed: false}
				return
			}
			results <- result{instanceID: instanceID, claimed: claimed}
		}(id)
	}

	winners := 0
	var winnerID string
	for i := 0; i < n; i++ {
		r := <-results
		if r.claimed {
			winners++
			winnerID = r.instanceID
		}
	}

	if winners != 1 {
		t.Fatalf("expected exactly 1 winner among %d concurrent takeover attempts, got %d", n, winners)
	}

	owner, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok {
		t.Fatal("expected an ownership record to exist after the race")
	}
	if owner != winnerID {
		t.Errorf("persisted owner %q does not match the reported winner %q", owner, winnerID)
	}
	if owner == deadOwner {
		t.Error("expected ownership to have moved away from the dead owner")
	}
}

// TestRedisDirectory_ConcurrentRenewOwnership_LoserCannotResurrectClaim
// covers RenewOwnership's own doc-commented guarantee directly: once
// ownership has moved to a new owner, the old owner's own (possibly
// in-flight, possibly just running late) heartbeat-renewal call must not be
// able to resurrect its stale claim.
func TestRedisDirectory_ConcurrentRenewOwnership_LoserCannotResurrectClaim(t *testing.T) {
	flushTestRedisDB(t)
	d := newTestDirectory()
	ctx := context.Background()
	gameID := uuid.NewString()

	if _, err := d.ClaimOwnership(ctx, gameID, "instance-a", ""); err != nil {
		t.Fatalf("initial claim: %v", err)
	}
	if claimed, err := d.ClaimOwnership(ctx, gameID, "instance-b", "instance-a"); err != nil || !claimed {
		t.Fatalf("takeover: claimed=%v err=%v", claimed, err)
	}

	// instance-a's own heartbeat ticker, unaware it has already lost the
	// game, fires a renewal.
	renewed, err := d.RenewOwnership(ctx, gameID, "instance-a")
	if err != nil {
		t.Fatalf("RenewOwnership: %v", err)
	}
	if renewed {
		t.Fatal("expected the former owner's renewal to fail after a legitimate takeover")
	}

	owner, ok, err := d.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok || owner != "instance-b" {
		t.Errorf("expected owner to remain instance-b, got owner=%q ok=%v", owner, ok)
	}
}

// Sanity check that flushTestRedisDB only ever touches DB 1, not whatever a
// developer might have on DB 0 — a silent cross-DB wipe would be a serious,
// hard-to-notice bug in the test harness itself.
func TestFlushTestRedisDB_TargetsIsolatedDB(t *testing.T) {
	if testRedisClient.Options().DB != 1 {
		t.Fatalf("testRedisClient must use DB 1 for test isolation, got DB %d", testRedisClient.Options().DB)
	}
}
