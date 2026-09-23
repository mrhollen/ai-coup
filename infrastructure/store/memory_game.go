package store

import (
	"sync"

	"forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
)

// MemoryGameStore is the in-memory GameStore (spec §2.1): a mutex-guarded map
// of live game aggregates. The same *domain.Game pointers the application
// layer mutates (under its per-game locks) are stored here; All() feeds the
// future /debug endpoint.
type MemoryGameStore struct {
	mu    sync.RWMutex
	games map[string]*domain.Game
}

// NewMemoryGameStore builds an empty store.
func NewMemoryGameStore() *MemoryGameStore {
	return &MemoryGameStore{games: map[string]*domain.Game{}}
}

// Put registers (or replaces) the aggregate for game.ID.
func (s *MemoryGameStore) Put(game *domain.Game) {
	s.mu.Lock()
	s.games[game.ID] = game
	s.mu.Unlock()
}

// Get looks up a live game aggregate.
func (s *MemoryGameStore) Get(gameID string) (*domain.Game, bool) {
	s.mu.RLock()
	g, ok := s.games[gameID]
	s.mu.RUnlock()
	return g, ok
}

// All returns pointers to every stored game (debug facility; the application
// layer's per-game mutexes still guard aggregate access).
func (s *MemoryGameStore) All() []*domain.Game {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Game, 0, len(s.games))
	for _, g := range s.games {
		out = append(out, g)
	}
	return out
}

var _ application.GameStore = (*MemoryGameStore)(nil)
