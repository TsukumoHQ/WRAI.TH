package db

import (
	"encoding/json"
	"fmt"
	"time"
)

// Compiled guards at the ladder, slice S2b (design 4d2e57a3 §4.1, §7, ruling
// 3ece19c0). match_precedent evaluates ladder-point guards on the systemic in
// the same writer tx as the rung transition, and the human rung compiles a
// generalized reply into a shadow guard in the tx that resolves the systemic.

// linkedAnchorTx is the newest instance linked to a systemic from its top
// raiser: an open-point guard generalized from a systemic matches instances,
// so its scope, class and replay are read from this one (§7 accept_known).
func linkedAnchorTx(q excQ, systemicID string) (excFacts, error) {
	fs, err := excFactsListTx(q, `e.cause_id = ? AND e.raised_by = (SELECT raised_by FROM exceptions WHERE cause_id = ?
		GROUP BY raised_by ORDER BY COUNT(*) DESC, MAX(opened_at) DESC LIMIT 1) ORDER BY e.opened_at DESC, e.id DESC LIMIT 1`,
		systemicID, systemicID)
	if err != nil {
		return excFacts{}, err
	}
	if len(fs) == 0 {
		return excFacts{}, guardErr(GuardErrNotFound, "systemic %s has no linked instance", systemicID)
	}
	return fs[0], nil
}

// MatchPrecedent runs the match_precedent rung: ladder-point guards are
// evaluated on the systemic (shadow guards record, at most one active guard
// acts), then, in the same tx, an acting resolve_systemic guard resolves the
// systemic (resolved_by = precedent), an acting route guard jumps to
// route_specialist carrying its profile (inserted after this rung when the
// snapshot has none ahead), and otherwise the ladder advances as before with
// the precedent as evidence. It returns the guard that acted, if any.
func (d *DB) MatchPrecedent(a ActiveRung, now time.Time) (*Guard, error) {
	ev := map[string]any{"precedent": d.PrecedentFor(a.ReasonCode, a.ExceptionID)}
	tx, err := d.beginWriterTx()
	if err != nil {
		return nil, fmt.Errorf("precedent begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	g, err := evaluateGuardsTx(tx, GuardPointLadder, a.ExceptionID, now.UTC().Format(memoryTimeFmt))
	if err != nil {
		return nil, err
	}
	var ok bool
	switch {
	case g == nil:
		ok, err = d.advanceRungTx(tx, a, ObligationFulfilled, ev, a.Rung+1, "", now)
	case g.Action == GuardActionResolveSystemic:
		ev["guard_id"] = g.ID
		ok, err = resolveAtRungTx(tx, a, "precedent", g.ActionParams["reason"], ev, now)
	default: // GuardActionRoute
		ev["guard_id"], ev["route_profile"] = g.ID, g.ActionParams["profile"]
		var to int
		if a, to, err = d.routeSnapshotTx(tx, a, g.ActionParams["profile"]); err == nil {
			ok, err = d.advanceRungTx(tx, a, ObligationFulfilled, ev, to, "guard_route", now)
		}
	}
	if !ok || err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("precedent commit: %w", err)
	}
	return g, nil
}

// routeSnapshotTx sets params.profile on the next route_specialist rung of the
// frozen snapshot (inserting one right after the current rung when none is
// ahead) and returns the updated rung view and that rung's index.
func (d *DB) routeSnapshotTx(tx *writerTx, a ActiveRung, profile string) (ActiveRung, int, error) {
	snap := a.Snapshot
	snap.Rungs = append([]LadderRung(nil), a.Snapshot.Rungs...)
	to := -1
	for i := a.Rung + 1; i < len(snap.Rungs) && to < 0; i++ {
		if snap.Rungs[i].Rung == RungRouteSpecialist {
			to = i
		}
	}
	if to < 0 {
		to = a.Rung + 1
		r := LadderRung{Rung: RungRouteSpecialist, NormID: "exc." + RungRouteSpecialist,
			NormVersion: d.normVersion("exc." + RungRouteSpecialist)}
		for _, c := range excLadderRungs {
			if c.rung == RungRouteSpecialist {
				r.DeadlineS = c.deadlineS
			}
		}
		snap.Rungs = append(snap.Rungs[:to], append([]LadderRung{r}, snap.Rungs[to:]...)...)
	}
	params := map[string]string{"profile": profile}
	for k, v := range snap.Rungs[to].Params {
		if k != "profile" {
			params[k] = v
		}
	}
	snap.Rungs[to].Params = params
	raw, _ := json.Marshal(snap)
	res, err := tx.Exec(`UPDATE exceptions SET ladder_snapshot_json = ? WHERE id = ? AND rung = ? AND status = 'open'`,
		string(raw), a.ExceptionID, a.Rung)
	if err != nil {
		return a, 0, fmt.Errorf("route snapshot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return a, 0, fmt.Errorf("route snapshot: systemic %s moved", a.ExceptionID)
	}
	a.Snapshot = snap
	return a, to, nil
}

// ResolveHumanAtRung resolves the systemic on the human's answer and, when gc
// is set (generalize=true), compiles it into a shadow guard in the same tx
// (source human_generalize, created_by human). A refused compile never loses
// the human's decision: it is rolled back to a savepoint and its refusal is
// recorded on the rung's evidence as generalize_error.
func (d *DB) ResolveHumanAtRung(a ActiveRung, reason string, add map[string]any, gc *GuardCompile, now time.Time) (bool, error) {
	tx, err := d.beginWriterTx()
	if err != nil {
		return false, fmt.Errorf("resolve begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := resolveAtRungTx(tx, a, "human", reason, add, now)
	if !ok || err != nil {
		return false, err
	}
	if gc != nil {
		in := *gc
		in.ExceptionID, in.Caller, in.Source, in.Now = a.ExceptionID, "human", GuardSourceHuman, now
		if _, err := tx.Exec(`SAVEPOINT generalize`); err != nil {
			return false, err
		}
		if _, cerr := CompileResolutionTx(tx, in); cerr != nil {
			if _, err := tx.Exec(`ROLLBACK TO generalize`); err != nil {
				return false, err
			}
			m := map[string]any{}
			for k, v := range add {
				m[k] = v
			}
			m["generalize_error"] = cerr.Error()
			if _, err := tx.Exec(`UPDATE obligations SET discharge_evidence = ? WHERE id = ?`,
				mergeEvidence(a.Evidence, m), a.ObligationID); err != nil {
				return false, err
			}
		}
		if _, err := tx.Exec(`RELEASE generalize`); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("resolve commit: %w", err)
	}
	return true, nil
}

// GuardDemotion is one guard_demote audit event, for the after-commit notice
// to the guard's creator and challenger.
type GuardDemotion struct {
	GuardID, Project, Reason, CreatedBy, ChallengedBy, At string
}

// GuardDemotionsSince lists guard_demote audit events after since (created_at
// order). Every demotion writes one, inline or from the sweep, so a cursor
// over them notifies each demotion exactly once.
func (d *DB) GuardDemotionsSince(since string) ([]GuardDemotion, error) {
	rows, err := d.ro().Query(`SELECT resource_id, project, COALESCE(details, ''), created_at FROM audit_log
		WHERE action = 'guard_demote' AND resource_type = 'guard' AND created_at > ? ORDER BY created_at, id`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []GuardDemotion
	for rows.Next() {
		var g GuardDemotion
		var det string
		if err := rows.Scan(&g.GuardID, &g.Project, &det, &g.At); err != nil {
			return nil, err
		}
		var m struct {
			Reason       string `json:"reason"`
			CreatedBy    string `json:"created_by"`
			ChallengedBy string `json:"challenged_by"`
		}
		_ = json.Unmarshal([]byte(det), &m)
		g.Reason, g.CreatedBy, g.ChallengedBy = m.Reason, m.CreatedBy, m.ChallengedBy
		out = append(out, g)
	}
	return out, rows.Err()
}
