package db

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DecisionGraphMermaid renders the project's decision ledger (active + archived)
// as a GitHub-native mermaid flowchart (steal #11). Nodes are decisions; edges
// are the causal links: a solid `supersedes` edge to the prior decision it
// replaced, a dashed `depends` edge to each decision it rests on. Superseded
// nodes get the `archived` class so the live frontier is visible at a glance.
// A referenced key with no decision of its own is drawn as a stub node so a
// dangling/forward ref is visible rather than silently dropped.
func (d *DB) DecisionGraphMermaid(project string) (string, error) {
	decs, err := d.AllDecisions(project)
	if err != nil {
		return "", err
	}

	// Parse once; keep deterministic order (by key) so the output is stable.
	type node struct {
		key      string
		label    string
		archived bool
		real     bool
	}
	nodes := map[string]*node{}
	order := []string{}
	ensure := func(key string) *node {
		if n, ok := nodes[key]; ok {
			return n
		}
		n := &node{key: key}
		nodes[key] = n
		order = append(order, key)
		return n
	}

	type edge struct{ from, to, kind string }
	var edges []edge

	for _, m := range decs {
		var dv DecisionValue
		if json.Unmarshal([]byte(m.Value), &dv) != nil {
			continue
		}
		n := ensure(m.Key)
		n.real = true
		n.label = mermaidLabel(m.Key, dv.Decision)
		n.archived = m.ArchivedAt != nil || dv.Status == "superseded"
		if dv.Supersedes != "" {
			ensure(dv.Supersedes)
			edges = append(edges, edge{m.Key, dv.Supersedes, "supersedes"})
		}
		for _, dep := range dv.DependsOn {
			ensure(dep)
			edges = append(edges, edge{m.Key, dep, "depends"})
		}
	}

	sort.Strings(order)

	var b strings.Builder
	b.WriteString("graph TD\n")
	if len(order) == 0 {
		b.WriteString("  empty[\"no decisions yet\"]\n")
		return b.String(), nil
	}
	for _, key := range order {
		n := nodes[key]
		id := mermaidID(key)
		label := n.label
		if !n.real {
			label = mermaidLabel(key, "(referenced, no record)")
		}
		fmt.Fprintf(&b, "  %s[\"%s\"]\n", id, label)
	}
	for _, e := range edges {
		if e.kind == "supersedes" {
			fmt.Fprintf(&b, "  %s -->|supersedes| %s\n", mermaidID(e.from), mermaidID(e.to))
		} else {
			fmt.Fprintf(&b, "  %s -.->|depends| %s\n", mermaidID(e.from), mermaidID(e.to))
		}
	}
	// Style archived (superseded) nodes so the live frontier stands out.
	b.WriteString("  classDef archived stroke-dasharray:4 3,opacity:0.55\n")
	var archived []string
	for _, key := range order {
		if nodes[key].archived {
			archived = append(archived, mermaidID(key))
		}
	}
	if len(archived) > 0 {
		fmt.Fprintf(&b, "  class %s archived\n", strings.Join(archived, ","))
	}
	return b.String(), nil
}

// mermaidID sanitizes a DEC key into a mermaid-safe node id (alnum + underscore).
func mermaidID(key string) string {
	var sb strings.Builder
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// mermaidLabel builds a node label "KEY: <short decision>", escaping quotes and
// collapsing whitespace, truncated rune-safely so the graph stays legible.
func mermaidLabel(key, decision string) string {
	text := strings.Join(strings.Fields(decision), " ")
	const max = 60
	if r := []rune(text); len(r) > max {
		text = string(r[:max]) + "…"
	}
	text = strings.ReplaceAll(text, `"`, "'")
	if text == "" {
		return key
	}
	return key + ": " + text
}
