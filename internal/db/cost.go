package db

import (
	"fmt"
	"sort"
	"strings"
)

// modelRate is a model tier's price per MILLION tokens (input / output), from
// claude.com pricing (2026): Opus $5/$25, Sonnet $3/$15, Haiku $1/$5.
type modelRate struct {
	inPerMTok, outPerMTok float64
}

var modelRates = map[string]modelRate{
	"opus":   {5, 25},
	"sonnet": {3, 15},
	"haiku":  {1, 5},
}

// rateForModel resolves a model string to its tier by substring (e.g.
// "claude-opus-4-8" → opus). Falls back to the configured default tier, then opus.
func rateForModel(model, fallback string) modelRate {
	m := strings.ToLower(model)
	for tier, r := range modelRates {
		if strings.Contains(m, tier) {
			return r
		}
	}
	if r, ok := modelRates[strings.ToLower(fallback)]; ok {
		return r
	}
	return modelRates["opus"]
}

// costUSD computes a turn's dollar cost at a tier. Cache reads bill at 0.1×
// input and 5-min cache writes at 1.25× input (the conservative default) — NOT
// flat input price, or long-running cached agents are massively over-reported.
// Output bills at the output rate. Rates are per million tokens.
func costUSD(r modelRate, input, output, cacheRead, cacheCreation int64) float64 {
	const m = 1_000_000.0
	return (float64(input)*r.inPerMTok +
		float64(cacheRead)*r.inPerMTok*0.1 +
		float64(cacheCreation)*r.inPerMTok*1.25 +
		float64(output)*r.outPerMTok) / m
}

// AgentCost is the per-agent token + dollar rollup over a window.
type AgentCost struct {
	Agent  string  `json:"agent"`
	Tokens int64   `json:"tokens"`
	USD    float64 `json:"usd"`
}

// GetCostByAgent rolls up real-token usage into $ per agent since a time, pricing
// each (agent, model) group at its tier (rows with no model use the configured
// default tier, setting "cost_default_model", else opus). Legacy bytes-only rows
// (pre-real-counts) contribute 0 $ — cost reflects measured tokens. Sorted by $ desc.
func (d *DB) GetCostByAgent(project, since string) ([]AgentCost, error) {
	fallback := strings.TrimSpace(d.GetSetting("cost_default_model"))
	if fallback == "" {
		fallback = "opus"
	}
	rows, err := d.ro().Query(`
		SELECT agent, COALESCE(model, ''),
		       SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens), SUM(cache_creation_tokens)
		FROM token_usage
		WHERE project = ? AND created_at >= ?
		GROUP BY agent, model`,
		project, since,
	)
	if err != nil {
		return nil, fmt.Errorf("cost by agent: %w", err)
	}
	defer func() { _ = rows.Close() }()

	agg := map[string]*AgentCost{}
	for rows.Next() {
		var agent, model string
		var in, out, cr, cc int64
		if err := rows.Scan(&agent, &model, &in, &out, &cr, &cc); err != nil {
			return nil, fmt.Errorf("scan cost: %w", err)
		}
		a := agg[agent]
		if a == nil {
			a = &AgentCost{Agent: agent}
			agg[agent] = a
		}
		a.Tokens += in + out + cr + cc
		a.USD += costUSD(rateForModel(model, fallback), in, out, cr, cc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]AgentCost, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].USD > out[j].USD })
	return out, nil
}
