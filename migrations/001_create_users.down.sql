-- Migration 001 DOWN: drop users
-- games references users, so games must be dropped first (handled by migration 002 down).
-- Running this down migration without first running 002 down will fail due to FK constraint.
-- golang-migrate runs down migrations one at a time in reverse order, so this is correct.

DROP TABLE IF EXISTS users;