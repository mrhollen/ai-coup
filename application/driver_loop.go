package application

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// afterBatch is the post-append orchestration hook, run under the game lock
// after every committed batch (spec §5.2.6, §7, §9): it logs the batch (§13),
// cancels stale in-flight LLM decisions, reconciles the open structure's
// timer/heartbeat bookkeeping, and dispatches drivers for every LLM seat that
// now owes a decision.
func (h *gameHandle) afterBatch(g *domain.Game, evs []domain.Event) {
	info := scanBatch(g, evs)
	h.logBatch(g, evs, info)
	h.cancelStaleFlightsLocked(g, info)
	h.reconcileStructureLocked(g, evs, info)
	h.drivePendingLocked(g, info)
}

// batchInfo is what a committed batch tells the orchestrator about structural
// changes.
type batchInfo struct {
	closedWindows map[string]string // windowID → close reason
	eliminated    map[string]bool   // seat IDs eliminated (by play or forfeit)
	gameOver      bool
	newTrigger    string // public summary of a structure this batch opened
}

func scanBatch(g *domain.Game, evs []domain.Event) batchInfo {
	info := batchInfo{closedWindows: map[string]string{}, eliminated: map[string]bool{}}
	bySeq := make(map[int64]domain.Event, len(evs))
	for _, e := range evs {
		bySeq[e.Seq] = e
	}
	for _, e := range evs {
		switch e.Type {
		case "window_closed":
			if wid, _ := e.Payload["window_id"].(string); wid != "" {
				reason, _ := e.Payload["reason"].(string)
				info.closedWindows[wid] = reason
			}
		case "eliminated":
			if sid, _ := e.Payload["seat_id"].(string); sid != "" {
				info.eliminated[sid] = true
			}
		case "game_over":
			info.gameOver = true
		case "window_opened", "card_flip_choice_requested", "exchange_revealed":
			if info.newTrigger == "" {
				info.newTrigger = structureTrigger(g, e, bySeq)
			}
		}
	}
	return info
}

// structureTrigger derives the public "what is being asked" summary for a
// newly opened structure from events already in the same batch (spec §6.4
// compact world render — public facts only).
func structureTrigger(g *domain.Game, e domain.Event, bySeq map[int64]domain.Event) string {
	if e.Type != "window_opened" {
		switch e.Type {
		case "card_flip_choice_requested":
			return "Choose which of your two face-down cards flips face-up."
		default: // exchange_revealed
			return "Finish your exchange: keep neither drawn card, or swap one drawn card for one of yours."
		}
	}
	trigger, _ := e.Payload["trigger"].(map[string]any)
	seq, _ := trigger["action_seq"].(int64)
	t, ok := bySeq[seq]
	if !ok {
		return "A claim has been made; respond if you wish."
	}
	switch t.Type {
	case "action_declared":
		if text, _ := t.Payload["text"].(string); text != "" {
			return text
		}
	case "response_received":
		resp, _ := t.Payload["response"].(map[string]any)
		if respType, _ := resp["type"].(string); respType == "counterclaim" {
			voter, _ := t.Payload["seat_id"].(string)
			name := seatName(g, voter)
			char, _ := resp["character"].(string)
			return name + " counterclaims " + char + " to block the attack."
		}
	}
	return "A claim has been made; respond if you wish."
}

func seatName(g *domain.Game, id string) string {
	for i := range g.Seats {
		if g.Seats[i].ID == id {
			return g.Seats[i].Name
		}
	}
	return "?"
}

// logBatch emits the §13 observability lines for a batch: one INFO "state"
// line per event with per-event type + payload deltas (events carry the state
// changes), plus decision-lifecycle lines for window/decision openings,
// responses and closings. The caller holds h.mu.
func (h *gameHandle) logBatch(g *domain.Game, evs []domain.Event, info batchInfo) {
	now := h.app.deps.Clock.Now()
	for _, e := range evs {
		h.logEventState(g, e)
		switch e.Type {
		case "window_opened":
			wid, _ := e.Payload["window_id"].(string)
			h.app.log.Info("window_opened", append(logGameContext(h.id, g.TurnID, e.Seq),
				"window_id", wid,
				"kind", payloadString(e.Payload, "kind"),
				"eligible", payloadSlice(e.Payload, "eligible"),
				"require_all_responses", payloadBool(e.Payload, "require_all_responses"),
				"timeout_ms", payloadInt(e.Payload, "timeout_ms"))...)
			if h.meta != nil && h.meta.id == wid {
				h.app.log.Info("window_opened_deadline", append(logGameContext(h.id, g.TurnID, e.Seq),
					"window_id", wid, "deadline_at", now.Add(time.Duration(h.meta.timeoutMs)*time.Millisecond).UTC().Format(time.RFC3339))...)
			}
		case "response_received":
			wid, _ := e.Payload["window_id"].(string)
			resp, _ := e.Payload["response"].(map[string]any)
			kind, _ := resp["type"].(string)
			line := []any{"game_id", h.id, "turn_id", g.TurnID, "event_seq", e.Seq,
				"window_id", wid, "seat_id", payloadString(e.Payload, "seat_id"), "response", kind}
			if c, _ := resp["character"].(string); c != "" {
				line = append(line, "character", c)
			}
			if h.meta != nil && h.meta.id == wid {
				line = append(line, "elapsed_ms", now.Sub(h.meta.openedAt).Milliseconds())
			}
			if driver, ok := h.driverForSeat(g, payloadString(e.Payload, "seat_id")); ok {
				line = append(line, "driver", driver)
			}
			h.app.log.Info("window_response", line...)
		case "window_closed":
			wid, _ := e.Payload["window_id"].(string)
			line := []any{"game_id", h.id, "turn_id", g.TurnID, "event_seq", e.Seq,
				"window_id", wid, "reason", payloadString(e.Payload, "reason"),
				"decider_seat_id", payloadString(e.Payload, "decider_seat_id")}
			if h.meta != nil && h.meta.id == wid {
				line = append(line, "total_elapsed_ms", now.Sub(h.meta.openedAt).Milliseconds())
			}
			h.app.log.Info("window_closed", line...)
		case "card_flip_choice_requested", "exchange_revealed":
			var decID, kind string
			if dec, ok := e.Payload["decision"].(map[string]any); ok {
				decID, _ = dec["id"].(string)
				kind, _ = dec["kind"].(string)
			}
			h.app.log.Info("decision_opened", append(logGameContext(h.id, g.TurnID, e.Seq),
				"decision_id", decID, "kind", kind,
				"seat_id", payloadString(e.Payload, "seat_id"),
				"timeout_ms", h.metaTimeout())...)
		}
	}
}

func (h *gameHandle) metaTimeout() int {
	if h.meta != nil {
		return h.meta.timeoutMs
	}
	return 0
}

// logEventState is the per-event "state" line of §12: consistent context
// fields + transition type + the event's own payload deltas.
func (h *gameHandle) logEventState(g *domain.Game, e domain.Event) {
	line := append(logGameContext(h.id, g.TurnID, e.Seq), "transition", e.Type)
	if pid := eventPlayer(e); pid != "" {
		line = append(line, "player_id", pid)
	}
	if driver, ok := h.driverForSeat(g, eventPlayer(e)); ok {
		line = append(line, "driver", driver)
	}
	for _, k := range stateScalarKeys(e.Type) {
		v, ok := e.Payload[k]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			line = append(line, k, val)
		case int:
			line = append(line, k, val)
		case bool:
			line = append(line, k, val)
		}
	}
	h.app.log.Info("state", line...)
	if h.app.log.Enabled(context.Background(), slog.LevelDebug) {
		h.app.log.Debug("event_payload", append(logGameContext(h.id, g.TurnID, e.Seq), "transition", e.Type, "payload", e.Payload)...)
	}
}

// stateScalarKeys lists the payload fields worth carrying on the INFO state
// line per event type (the rest stays at DEBUG to keep lines readable).
func stateScalarKeys(eventType string) []string {
	switch eventType {
	case "action_declared":
		return []string{"actor_id", "text"} // full action detail rides in the public text
	case "coins_changed":
		return []string{"seat_id", "delta", "total", "reason"}
	case "card_flipped":
		return []string{"seat_id", "card", "reason"}
	case "claim_succeeded", "claim_failed":
		return []string{"seat_id", "challenger_seat_id"}
	case "action_fizzled":
		return []string{"actor_id", "action_seq", "reason"}
	case "eliminated":
		return []string{"seat_id", "reason"}
	case "game_over":
		return []string{"winner_seat_id"}
	case "forfeit_vote_opened":
		return []string{"vote_id", "target_seat_id", "initiator_seat_id", "majority_needed"}
	case "forfeit_vote_cast":
		return []string{"vote_id", "voter_seat_id"}
	case "forfeit_vote_resolved":
		return []string{"vote_id", "result"}
	case "window_opened":
		return []string{"window_id", "kind"}
	case "response_received":
		return []string{"window_id", "seat_id"}
	case "window_closed":
		return []string{"window_id", "reason", "decider_seat_id"}
	default:
		return nil
	}
}

// eventPlayer derives the consistent §13 player_id field from an event.
func eventPlayer(e domain.Event) string {
	for _, k := range []string{"seat_id", "actor_id", "voter_seat_id", "decider_seat_id", "initiator_seat_id", "challenger_seat_id"} {
		if s, ok := e.Payload[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// driverForSeat reports the driver name for an LLM seat (the consistent §13
// "driver" field). The caller holds h.mu.
func (h *gameHandle) driverForSeat(g *domain.Game, seatID string) (string, bool) {
	if seatID == "" {
		return "", false
	}
	for i := range g.Seats {
		if g.Seats[i].ID == seatID && g.Seats[i].Type == domain.SeatLLM {
			return h.app.driver.Name(), true
		}
	}
	return "", false
}

func payloadString(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func payloadBool(m map[string]any, k string) bool     { b, _ := m[k].(bool); return b }
func payloadInt(m map[string]any, k string) int       { i, _ := m[k].(int); return i }

func payloadSlice(m map[string]any, k string) []string {
	out := []string{}
	if v, ok := m[k].([]string); ok {
		out = append(out, v...)
	}
	return out
}

// cancelStaleFlightsLocked cancels in-flight LLM decisions made stale by the
// batch (spec §5.2.6, §9): a window that just closed, an eliminated/forfeited
// seat, or game over. One INFO "llm_cancelled" line per cancellation with
// seat, structure, driver and reason (§12.3). The caller holds h.mu.
func (h *gameHandle) cancelStaleFlightsLocked(g *domain.Game, info batchInfo) {
	for _, f := range h.inFlight {
		var reason string
		switch {
		case info.gameOver:
			reason = "game_over"
		case info.eliminated[f.seatID]:
			reason = "seat_eliminated"
		case f.kind == KindWindowResponse && info.closedWindows[f.structID] != "":
			reason = "window_closed:" + info.closedWindows[f.structID]
		}
		if reason != "" {
			h.cancelFlightLocked(f, reason)
		}
	}
}

func (h *gameHandle) cancelFlightLocked(f *flight, reason string) {
	if f.cancelled {
		return
	}
	f.cancelled = true
	f.cancel()
	// Only LLM seats carry in-flight drivers, so the driver field is the
	// configured MoveDriver name by construction (§12.3).
	h.app.log.Info("llm_cancelled", []any{"game_id", h.id, "player_id", f.seatID, "window", f.structID, "driver", h.app.driver.Name(), "reason", reason}...)
}

// reconcileStructureLocked updates the open-structure bookkeeping (timer,
// heartbeat, trigger summary) to match the engine state after the batch. The
// caller holds h.mu.
func (h *gameHandle) reconcileStructureLocked(g *domain.Game, evs []domain.Event, info batchInfo) {
	var newMeta *structureMeta
	switch g.Phase() {
	case domain.PhaseWindowOpen:
		wv := g.WindowView()
		if wv != nil {
			newMeta = &structureMeta{
				id:        wv.ID,
				kind:      "window",
				openedAt:  h.app.deps.Clock.Now(),
				timeoutMs: wv.Settings.TimeoutMs,
				trigger:   info.newTrigger,
				stop:      make(chan struct{}),
			}
		}
	case domain.PhaseSingleSeatDecision:
		dv := g.DecisionView()
		if dv != nil {
			newMeta = &structureMeta{
				id:        dv.ID,
				kind:      "decision",
				openedAt:  h.app.deps.Clock.Now(),
				timeoutMs: dv.TimeoutMs,
				trigger:   info.newTrigger,
				stop:      make(chan struct{}),
			}
		}
	}
	if h.meta != nil && (newMeta == nil || newMeta.id != h.meta.id) {
		close(h.meta.stop)
		delete(h.done, h.meta.id)
	}
	h.meta = newMeta
	if newMeta != nil {
		h.startStructureWatches(newMeta)
	}
}

// startStructureWatches spawns the timeout timer (when the structure honors a
// timeout) and the A11 still-waiting heartbeat for an open structure. The
// Clock.After channels are armed synchronously here — under the game lock,
// before any batch caller can release it — so a deadline registered for this
// structure can never lose a race against an Advance made immediately after
// the batch commits; only the wait-and-act happens in goroutines.
func (h *gameHandle) startStructureWatches(m *structureMeta) {
	stop := m.stop
	var tick <-chan time.Time
	if m.timeoutMs > 0 {
		tick = h.app.deps.Clock.After(time.Duration(m.timeoutMs) * time.Millisecond)
		go func() {
			select {
			case <-stop:
				return
			case <-tick:
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.meta == nil || h.meta.id != m.id || h.g.Status == domain.Finished {
				return // the structure resolved first; nothing to expire
			}
			h.app.log.Info("structure_timeout", []any{"game_id", h.id, "kind", m.kind, "id", m.id, "timeout_ms", m.timeoutMs}...)
			evs, err := h.g.ExpireWindow()
			if err != nil {
				return // CodeNothingToExpire: resolved between check and call
			}
			if _, aerr := h.commitLocked(h.g, evs); aerr != nil {
				h.app.log.Error("expire_commit_failed", "game_id", h.id, "err", aerr)
			}
		}()
	}
	go func() {
		tick := h.app.deps.Clock.After(heartbeatInterval)
		for {
			select {
			case <-stop:
				return
			case <-tick:
			}
			h.mu.Lock()
			if h.meta == nil || h.meta.id != m.id {
				h.mu.Unlock()
				return
			}
			turnID := h.g.TurnID
			var unresponded []string
			switch m.kind {
			case "window":
				if wv := h.g.WindowView(); wv != nil {
					for _, id := range wv.Eligible {
						if _, ok := wv.Responses[id]; !ok {
							unresponded = append(unresponded, id)
						}
					}
				}
			default: // decision
				if dv := h.g.DecisionView(); dv != nil {
					unresponded = []string{dv.SeatID}
				}
			}
			elapsed := h.app.deps.Clock.Now().Sub(m.openedAt)
			h.mu.Unlock()
			h.app.log.Info("still_waiting", []any{"game_id", h.id, "turn_id", turnID, "structure", m.kind, "id", m.id, "unresponded", unresponded, "elapsed_ms", elapsed.Milliseconds()}...)
			tick = h.app.deps.Clock.After(heartbeatInterval) // re-arm the next cycle
		}
	}()
}

// drivePendingLocked dispatches the MoveDriver for every active LLM seat that
// now owes a decision (turn action, window response, single-seat choice). A
// seat is dispatched at most once per structure and never has two concurrent
// Decide calls (§9). The caller holds h.mu.
func (h *gameHandle) drivePendingLocked(g *domain.Game, info batchInfo) {
	switch g.Phase() {
	case domain.PhaseFinished:
		return
	case domain.PhaseTurnAction:
		seat := &g.Seats[g.TurnSeatIndex]
		structID := "turn:" + strconv.Itoa(g.TurnID)
		if seat.Type == domain.SeatLLM && !h.isDone(structID, seat.ID) {
			h.dispatchLocked(g, seat.ID, KindTurnAction, Request{}, structID)
		}
	case domain.PhaseWindowOpen:
		wv := g.WindowView()
		if wv == nil {
			return
		}
		trigger := ""
		if h.meta != nil && h.meta.id == wv.ID {
			trigger = h.meta.trigger
		}
		for _, id := range wv.Eligible {
			seat := lookupSeat(g, id)
			if seat == nil || seat.Type != domain.SeatLLM || h.isDone(wv.ID, id) {
				continue
			}
			req := Request{WindowID: wv.ID, TriggerSummary: trigger, EligibleActions: g.AllowedResponsesFor(id)}
			h.dispatchLocked(g, id, KindWindowResponse, req, wv.ID)
		}
	case domain.PhaseSingleSeatDecision:
		dv := g.DecisionView()
		if dv == nil {
			return
		}
		if h.isDone(dv.ID, dv.SeatID) {
			return
		}
		// A decision addressed to a HUMAN seat is answered by that client —
		// the driver must never drive it (§6.3: drivers only occupy LLM seats).
		if seat := lookupSeat(g, dv.SeatID); seat == nil || seat.Type != domain.SeatLLM {
			return
		}
		req := Request{ChoiceKind: dv.Kind.String(), Options: choiceOptions(dv)}
		h.dispatchLocked(g, dv.SeatID, KindSingleSeatChoice, req, dv.ID)
	}
}

// lookupSeat finds a seat by id (the domain's own finder is private). The
// caller holds h.mu, so the aggregate is stable.
func lookupSeat(g *domain.Game, id string) *domain.Seat {
	for i := range g.Seats {
		if g.Seats[i].ID == id {
			return &g.Seats[i]
		}
	}
	return nil
}

// choiceOptions renders the option identifiers for a single-seat decision
// (§5.4): flip choices are positional; exchange finish offers one identifier
// per outcome — "no_swap" plus keep-drawn-i-return-hand-j combinations.
func choiceOptions(dv *domain.DecisionView) []string {
	switch dv.Kind {
	case domain.DecisionFlipChoice:
		return append([]string{}, dv.Options...)
	default: // DecisionExchangeFinish
		opts := []string{"no_swap"}
		for i := 0; i < len(dv.DrawnCards); i++ {
			for _, handOpt := range []string{"face_down_0", "face_down_1"} {
				opts = append(opts, "swap_drawn_"+strconv.Itoa(i)+"_"+handOpt)
			}
		}
		return opts
	}
}

func (h *gameHandle) isDone(structID, seatID string) bool {
	set, ok := h.done[structID]
	return ok && set[seatID]
}

func (h *gameHandle) markDone(structID, seatID string) {
	if h.done[structID] == nil {
		h.done[structID] = map[string]bool{}
	}
	h.done[structID][seatID] = true
}

// dispatchLocked registers and launches one driver decision for a seat. The
// caller holds h.mu; Decide itself runs outside the lock in its own goroutine.
func (h *gameHandle) dispatchLocked(g *domain.Game, seatID string, kind DecisionKind, req Request, structID string) {
	if h.inFlight[seatID] != nil { // §9: one in flight per seat
		return
	}
	var tail int
	if kind == KindTurnAction {
		tail = 10 // A5: bound the "recent events" prompt tail for turn decisions
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &flight{
		seatID:    seatID,
		structID:  structID,
		kind:      kind,
		ctx:       ctx,
		cancel:    cancel,
		startedAt: h.app.deps.Clock.Now(),
	}
	h.inFlight[seatID] = f
	h.markDone(structID, seatID)
	in := DecisionInput{
		GameID:  h.id,
		SeatID:  seatID,
		Kind:    kind,
		World:   h.worldView(g, seatID, tail),
		Request: req,
	}
	h.app.log.Debug("llm_dispatch", []any{"game_id", h.id, "player_id", seatID, "kind", kind.String(), "structure", structID}...)
	go h.runDecision(f, in)
}

// redispatchIfOwedLocked re-checks, after a flight released its per-seat slot,
// whether that seat owes a decision for the structure now open (spec §9: at
// most one concurrent Decide per seat). A turn or response that became owed
// WHILE a dying (just-cancelled) flight still held the slot would otherwise be
// lost: drivePendingLocked skips seats with an occupied slot and nothing else
// re-examines them. The caller holds h.mu; dispatch marks the structure done
// before launching, so this can never loop on the same structure.
func (h *gameHandle) redispatchIfOwedLocked(g *domain.Game, seatID string) {
	if g.Status == domain.Finished || h.inFlight[seatID] != nil {
		return
	}
	switch g.Phase() {
	case domain.PhaseTurnAction:
		seat := &g.Seats[g.TurnSeatIndex]
		if seat.ID != seatID || seat.Type != domain.SeatLLM {
			return
		}
		structID := "turn:" + strconv.Itoa(g.TurnID)
		if !h.isDone(structID, seatID) {
			h.dispatchLocked(g, seatID, KindTurnAction, Request{}, structID)
		}
	case domain.PhaseWindowOpen:
		wv := g.WindowView()
		if wv == nil {
			return
		}
		var owed bool
		for _, id := range wv.Eligible {
			if id == seatID {
				owed = true
				break
			}
		}
		if !owed || h.isDone(wv.ID, seatID) {
			return
		}
		trigger := ""
		if h.meta != nil && h.meta.id == wv.ID {
			trigger = h.meta.trigger
		}
		req := Request{WindowID: wv.ID, TriggerSummary: trigger, EligibleActions: g.AllowedResponsesFor(seatID)}
		h.dispatchLocked(g, seatID, KindWindowResponse, req, wv.ID)
	case domain.PhaseSingleSeatDecision:
		dv := g.DecisionView()
		if dv == nil || dv.SeatID != seatID || h.isDone(dv.ID, seatID) {
			return
		}
		req := Request{ChoiceKind: dv.Kind.String(), Options: choiceOptions(dv)}
		h.dispatchLocked(g, seatID, KindSingleSeatChoice, req, dv.ID)
	}
}

// worldView builds exactly what the seat may know (§8): its own hand and
// coins, every seat's public info, the Court Deck size, and the seat-visible
// event history (public + own private), optionally tail-capped. The caller
// holds h.mu.
func (h *gameHandle) worldView(g *domain.Game, seatID string, tail int) WorldView {
	wv := WorldView{TurnID: g.TurnID, DeckSize: g.Deck.Size(), Seats: make([]SeatView, 0, len(g.Seats))}
	for i := range g.Seats {
		s := &g.Seats[i]
		sv := SeatView{
			ID:     s.ID,
			Name:   s.Name,
			Type:   s.Type.String(),
			Coins:  s.Coins,
			FaceUp: characters(s.FaceUp),
			Active: s.Active(),
		}
		wv.Seats = append(wv.Seats, sv)
		if s.ID == seatID {
			wv.You = sv
			wv.YourCards = characters(s.FaceDown)
		}
	}
	hist, _ := h.app.deps.Events.Range(context.Background(), h.id, 0, seatID)
	if tail > 0 && len(hist) > tail {
		hist = hist[len(hist)-tail:]
	}
	wv.History = hist
	return wv
}

func characters(cards []domain.Card) []domain.Character {
	out := make([]domain.Character, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Character)
	}
	return out
}
