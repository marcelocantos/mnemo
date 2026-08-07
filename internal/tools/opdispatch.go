// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"fmt"
	"sort"
	"strings"
)

// Op-dispatch convention (🎯T143.2).
//
// A consolidated tool replaces several narrow tools with one entry
// point that takes an `op` parameter naming the operation. This file is
// the normative definition of how that works, so a folded subsystem is
// not three different designs.
//
// THE RULES.
//
//  1. Every consolidated tool declares an opTable: one opSpec per
//     operation, carrying the op's name, a one-line description, and
//     the exact set of parameters that op honours.
//  2. A missing or unknown op is an error naming the valid ops. An
//     agent that guesses wrong learns the answer from the failure
//     rather than from re-reading the description.
//  3. Parameters are validated PER OP, not unioned into one permissive
//     bag. A parameter belonging to a different op is rejected, naming
//     both the parameter and the ops that do accept it.
//  4. The tool description enumerates the ops via opTable.describe(),
//     so the enumeration cannot drift from the dispatch table.
//
// WHY RULE 3 MATTERS MOST. The tempting shortcut is to declare every
// parameter of every op on the tool and sort it out in the handler.
// That makes all ops look identical in the schema, so an agent cannot
// tell which parameters its chosen op actually honours — and a wrong
// guess is then accepted and silently ignored rather than refused. A
// consolidated tool that fails silently is worse than the narrow tools
// it replaced, which at least could not accept a parameter they did
// not have.
//
// WHEN NOT TO CONSOLIDATE.
//
// Consolidation trades discoverability for surface area, and the trade
// is only good when the discoverability being spent is not being used.
// Ten narrow tools have ten names an agent can scan; one tool with ten
// ops has one name and a paragraph. For a subsystem with no callers
// that costs nothing. For a subsystem in constant use it costs real
// legibility.
//
// The standing example is mnemo_search: 55% of all agent calls to
// mnemo in the 2026-08-07 audit. Folding it into a general tool behind
// op=search would take the single most-used affordance in the product
// and hide it. Do not. The same reasoning kept mnemo_status and
// mnemo_stats out of the operational consolidation (🎯T143.5).
//
// The test to apply: if agents are already finding and using a tool,
// its name is doing work, and an op is a worse name.

// opSpec describes one operation of a consolidated tool.
type opSpec struct {
	// name is the value callers pass as `op`.
	name string
	// desc is a one-line summary used to build the tool description.
	desc string
	// params is the exact set of parameter names this op honours,
	// excluding "op" itself. A parameter outside this set is rejected
	// rather than ignored (rule 3).
	params []string
}

// opTable is the dispatch table for one consolidated tool.
type opTable struct {
	// tool is the consolidated tool's name, used in error messages.
	tool string
	ops  []opSpec
}

// names returns the valid op names in declaration order.
func (t opTable) names() []string {
	out := make([]string, 0, len(t.ops))
	for _, o := range t.ops {
		out = append(out, o.name)
	}
	return out
}

// describe renders the op enumeration for the tool's description, so
// what an agent reads and what the dispatcher accepts come from one
// source and cannot drift.
func (t opTable) describe() string {
	var b strings.Builder
	for _, o := range t.ops {
		b.WriteString("\n  op=")
		b.WriteString(o.name)
		b.WriteString(" — ")
		b.WriteString(o.desc)
		if len(o.params) > 0 {
			b.WriteString(" (")
			b.WriteString(strings.Join(o.params, ", "))
			b.WriteString(")")
		}
	}
	return b.String()
}

// spec returns the named op's spec.
func (t opTable) spec(name string) (opSpec, bool) {
	for _, o := range t.ops {
		if o.name == name {
			return o, true
		}
	}
	return opSpec{}, false
}

// acceptedBy lists the ops that honour the given parameter, so a
// misplaced parameter's error can point at where it does belong.
func (t opTable) acceptedBy(param string) []string {
	var out []string
	for _, o := range t.ops {
		for _, p := range o.params {
			if p == param {
				out = append(out, o.name)
				break
			}
		}
	}
	return out
}

// resolve validates the call against the table and returns the op name.
//
// It enforces rules 2 and 3: a missing or unknown op names the valid
// ops, and a parameter that belongs to a different op is refused rather
// than ignored. The returned error is a tool-level error (shown to the
// caller), never a transport error.
func (t opTable) resolve(args map[string]any) (string, error) {
	raw, present := args["op"]
	op, _ := raw.(string)
	op = strings.TrimSpace(op)
	if !present || op == "" {
		return "", fmt.Errorf("%s requires an op. Valid ops: %s",
			t.tool, strings.Join(t.names(), ", "))
	}
	spec, ok := t.spec(op)
	if !ok {
		return "", fmt.Errorf("%s: unknown op %q. Valid ops: %s",
			t.tool, op, strings.Join(t.names(), ", "))
	}

	allowed := map[string]bool{}
	for _, p := range spec.params {
		allowed[p] = true
	}
	var stray []string
	for k := range args {
		if k == "op" || allowed[k] {
			continue
		}
		stray = append(stray, k)
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		var detail []string
		for _, s := range stray {
			if owners := t.acceptedBy(s); len(owners) > 0 {
				detail = append(detail, fmt.Sprintf("%q (belongs to op=%s)",
					s, strings.Join(owners, "/")))
			} else {
				detail = append(detail, fmt.Sprintf("%q (not a parameter of %s)", s, t.tool))
			}
		}
		accepts := "no parameters"
		if len(spec.params) > 0 {
			accepts = strings.Join(spec.params, ", ")
		}
		return "", fmt.Errorf("%s op=%s does not accept %s. op=%s accepts: %s",
			t.tool, op, strings.Join(detail, ", "), op, accepts)
	}
	return op, nil
}
