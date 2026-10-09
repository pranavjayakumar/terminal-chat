//go:build race

package client

// raceSlowdown scales time budgets: the race detector makes the pure-Go
// SQLite 25-40x slower, so a budget meant for a normal build only guards
// against pathologies there.
const raceSlowdown = 40
