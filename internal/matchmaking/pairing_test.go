//go:build integration

package matchmaking

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
)

// fakeMatchReporter is a MatchReporter test double recording every call it
// receives, safe for concurrent use —
// TestPairingLoop_ConcurrentTicks_NoDoubleMatch calls into two separate
// instances of it from two goroutines simultaneously.
type fakeMatchReporter struct {
	mu      sync.Mutex
	created []matchCreatedCall
	failed  []matchFailedCall
}

type matchCreatedCall struct {
	gameID, whiteUserID, blackUserID, instanceLabel string
	tokens                                          game.MatchedGameTokens
}

type matchFailedCall struct {
	whiteUserID, blackUserID string
}

func (f *fakeMatchReporter) ReportMatchCreated(ctx context.Context, gameID, whiteUserID, blackUserID string, tokens game.MatchedGameTokens, instanceLabel string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, matchCreatedCall{gameID, whiteUserID, blackUserID, instanceLabel, tokens})
	return nil
}

func (f *fakeMatchReporter) ReportMatchmakingFailed(ctx context.Context, whiteUserID, blackUserID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, matchFailedCall{whiteUserID, blackUserID})
	return nil
}

func (f *fakeMatchReporter) createdCalls() []matchCreatedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]matchCreatedCall, len(f.created))
	copy(out, f.created)
	return out
}

func (f *fakeMatchReporter) failedCalls() []matchFailedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]matchFailedCall, len(f.failed))
	copy(out, f.failed)
	return out
}

func TestPairingLoop_Tick_FewerThanTwoInQueue_NoOp(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)
	ctx := context.Background()

	mustCreateUser(t, "10000000-0000-0000-0000-000000000001")
	if err := testRedisClient.ZAdd(ctx, queueKey,
		redis.Z{Score: 1, Member: "10000000-0000-0000-0000-000000000001"}).Err(); err != nil {
		t.Fatalf("ZADD: %v", err)
	}

	manager := newTestManager(t, "instance-1")
	reporter := &fakeMatchReporter{}
	loop := NewPairingLoop(testRedisClient, manager, reporter, time.Second, "instance-1")

	loop.tick(ctx)

	if got := reporter.createdCalls(); len(got) != 0 {
		t.Errorf("expected no ReportMatchCreated calls, got %d", len(got))
	}
	count, err := testRedisClient.ZCard(ctx, queueKey).Result()
	if err != nil {
		t.Fatalf("ZCARD: %v", err)
	}
	if count != 1 {
		t.Errorf("queue count: got %d, want 1 (single member should remain untouched)", count)
	}
}

func TestPairingLoop_Tick_PairsSuccessfully(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)
	ctx := context.Background()

	whiteID := "10000000-0000-0000-0000-000000000001"
	blackID := "10000000-0000-0000-0000-000000000002"
	mustCreateUser(t, whiteID)
	mustCreateUser(t, blackID)
	if err := testRedisClient.ZAdd(ctx, queueKey,
		redis.Z{Score: 100, Member: whiteID},
		redis.Z{Score: 200, Member: blackID},
	).Err(); err != nil {
		t.Fatalf("ZADD: %v", err)
	}

	manager := newTestManager(t, "instance-1")
	reporter := &fakeMatchReporter{}
	loop := NewPairingLoop(testRedisClient, manager, reporter, time.Second, "instance-1")

	loop.tick(ctx)

	created := reporter.createdCalls()
	if len(created) != 1 {
		t.Fatalf("expected exactly 1 ReportMatchCreated call, got %d", len(created))
	}
	call := created[0]
	if call.whiteUserID != whiteID || call.blackUserID != blackID {
		t.Errorf("player IDs: got white=%s black=%s, want white=%s black=%s",
			call.whiteUserID, call.blackUserID, whiteID, blackID)
	}
	if call.tokens.WhitePlayerToken == "" || call.tokens.BlackPlayerToken == "" {
		t.Error("expected non-empty PlayerClaims tokens for both players")
	}
	if call.tokens.WhiteConnectToken == "" || call.tokens.BlackConnectToken == "" {
		t.Error("expected non-empty ConnectClaims tokens for both players (PHASE_3_DESIGN_NOTES.md §18)")
	}
	if call.instanceLabel != "instance-1" {
		t.Errorf("instanceLabel: got %q, want %q", call.instanceLabel, "instance-1")
	}

	count, err := testRedisClient.ZCard(ctx, queueKey).Result()
	if err != nil {
		t.Fatalf("ZCARD: %v", err)
	}
	if count != 0 {
		t.Errorf("queue count: got %d, want 0 (both players should be dequeued)", count)
	}

	// DECISIONS_LOG_PHASE_3.md ADR-041/TD-P3-004: a matched game starts
	// WAITING_FOR_PLAYER, not ACTIVE — status only flips to ACTIVE once both
	// players actually connect over WebSocket.
	g, err := store.NewGameStore(testPool).GetGame(ctx, call.gameID)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if g.Status != store.GameStatusWaiting {
		t.Errorf("Status: got %q, want %q", g.Status, store.GameStatusWaiting)
	}
	if g.PlayerWhiteID != whiteID {
		t.Errorf("PlayerWhiteID: got %q, want %q", g.PlayerWhiteID, whiteID)
	}
	if g.PlayerBlackID == nil || *g.PlayerBlackID != blackID {
		t.Errorf("PlayerBlackID: got %v, want %q", g.PlayerBlackID, blackID)
	}
}

func TestPairingLoop_Tick_OddQueue_OneRemains(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)
	ctx := context.Background()

	userA := "10000000-0000-0000-0000-000000000001"
	userB := "10000000-0000-0000-0000-000000000002"
	userC := "10000000-0000-0000-0000-000000000003"
	mustCreateUser(t, userA)
	mustCreateUser(t, userB)
	mustCreateUser(t, userC)
	if err := testRedisClient.ZAdd(ctx, queueKey,
		redis.Z{Score: 100, Member: userA},
		redis.Z{Score: 200, Member: userB},
		redis.Z{Score: 300, Member: userC},
	).Err(); err != nil {
		t.Fatalf("ZADD: %v", err)
	}

	manager := newTestManager(t, "instance-1")
	reporter := &fakeMatchReporter{}
	loop := NewPairingLoop(testRedisClient, manager, reporter, time.Second, "instance-1")

	loop.tick(ctx)

	created := reporter.createdCalls()
	if len(created) != 1 {
		t.Fatalf("expected exactly 1 ReportMatchCreated call, got %d", len(created))
	}
	// PHASE_3.md Challenge 2: the two LOWEST scores (earliest arrivals) are
	// matched — userA and userB, not userC.
	if created[0].whiteUserID != userA || created[0].blackUserID != userB {
		t.Errorf("expected userA+userB matched (lowest scores), got white=%s black=%s",
			created[0].whiteUserID, created[0].blackUserID)
	}

	count, err := testRedisClient.ZCard(ctx, queueKey).Result()
	if err != nil {
		t.Fatalf("ZCARD: %v", err)
	}
	if count != 1 {
		t.Errorf("queue count: got %d, want 1 (userC should remain)", count)
	}
	score, err := testRedisClient.ZScore(ctx, queueKey, userC).Result()
	if err != nil {
		t.Fatalf("ZSCORE userC: %v", err)
	}
	if score != 300 {
		t.Errorf("userC score: got %v, want 300 (unchanged)", score)
	}
}

// TestPairingLoop_ConcurrentTicks_NoDoubleMatch is this phase's actual
// learning objective under direct test (PHASE_3.md: "Distributed race
// conditions are real, silent, and dangerous. Atomic operations are the
// correct solution."). Two separate Managers/PairingLoops/reporters simulate
// two independent chess-server instances racing the SAME Redis queue and
// Postgres database — run with `go test -race`, ZPOPMIN's atomicity
// (Challenge 1) is the entire claim under test.
func TestPairingLoop_ConcurrentTicks_NoDoubleMatch(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)
	ctx := context.Background()

	whiteID := "10000000-0000-0000-0000-000000000001"
	blackID := "10000000-0000-0000-0000-000000000002"
	mustCreateUser(t, whiteID)
	mustCreateUser(t, blackID)
	if err := testRedisClient.ZAdd(ctx, queueKey,
		redis.Z{Score: 100, Member: whiteID},
		redis.Z{Score: 200, Member: blackID},
	).Err(); err != nil {
		t.Fatalf("ZADD: %v", err)
	}

	manager1 := newTestManager(t, "instance-1")
	manager2 := newTestManager(t, "instance-2")
	reporter1 := &fakeMatchReporter{}
	reporter2 := &fakeMatchReporter{}
	loop1 := NewPairingLoop(testRedisClient, manager1, reporter1, time.Second, "instance-1")
	loop2 := NewPairingLoop(testRedisClient, manager2, reporter2, time.Second, "instance-2")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); loop1.tick(ctx) }()
	go func() { defer wg.Done(); loop2.tick(ctx) }()
	wg.Wait()

	totalCreated := len(reporter1.createdCalls()) + len(reporter2.createdCalls())
	if totalCreated != 1 {
		t.Fatalf("expected exactly 1 ReportMatchCreated call across both instances, got %d — "+
			"this is exactly the double-match race PHASE_3.md's Key Technical Challenges section describes", totalCreated)
	}

	var gameCount int
	err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM games WHERE player_white_id = $1 AND player_black_id = $2`,
		whiteID, blackID).Scan(&gameCount)
	if err != nil {
		t.Fatalf("count games: %v", err)
	}
	if gameCount != 1 {
		t.Errorf("games table: got %d rows for this pair, want exactly 1", gameCount)
	}
}

func TestPairingLoop_Tick_CreateMatchedGameFails_ReenqueuesAndReportsFailure(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)
	ctx := context.Background()

	// Deliberately NOT calling mustCreateUser for either ID — CreateMatchedGame's
	// INSERT will fail its foreign-key constraint on player_white_id/
	// player_black_id (migrations/002_create_games.up.sql), giving a real,
	// repeatable CreateMatchedGame failure without mocking anything.
	whiteID := "99999999-0000-0000-0000-000000000001"
	blackID := "99999999-0000-0000-0000-000000000002"
	if err := testRedisClient.ZAdd(ctx, queueKey,
		redis.Z{Score: 100, Member: whiteID},
		redis.Z{Score: 200, Member: blackID},
	).Err(); err != nil {
		t.Fatalf("ZADD: %v", err)
	}

	manager := newTestManager(t, "instance-1")
	reporter := &fakeMatchReporter{}
	loop := NewPairingLoop(testRedisClient, manager, reporter, time.Second, "instance-1")
	loop.maxCreateAttempts = 1 // this failure is deterministic (FK violation) — no point retrying 3x in a test

	loop.tick(ctx)

	if got := reporter.createdCalls(); len(got) != 0 {
		t.Errorf("expected no ReportMatchCreated calls, got %d", len(got))
	}
	failed := reporter.failedCalls()
	if len(failed) != 1 {
		t.Fatalf("expected exactly 1 ReportMatchmakingFailed call, got %d", len(failed))
	}
	if failed[0].whiteUserID != whiteID || failed[0].blackUserID != blackID {
		t.Errorf("failed call player IDs: got white=%s black=%s, want white=%s black=%s",
			failed[0].whiteUserID, failed[0].blackUserID, whiteID, blackID)
	}

	// PHASE_3_DESIGN_NOTES.md §5's failure branch: both players must be back
	// in the queue at their ORIGINAL scores — a server-side failure must not
	// cost them their queue position.
	whiteScore, err := testRedisClient.ZScore(ctx, queueKey, whiteID).Result()
	if err != nil {
		t.Fatalf("ZSCORE white after failure: %v", err)
	}
	if whiteScore != 100 {
		t.Errorf("white score after re-enqueue: got %v, want 100 (original)", whiteScore)
	}
	blackScore, err := testRedisClient.ZScore(ctx, queueKey, blackID).Result()
	if err != nil {
		t.Fatalf("ZSCORE black after failure: %v", err)
	}
	if blackScore != 200 {
		t.Errorf("black score after re-enqueue: got %v, want 200 (original)", blackScore)
	}
}
