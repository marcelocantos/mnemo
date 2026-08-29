// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"strings"
)

// ExpandCodexPatch parses Codex apply_patch text into zero or more ops.
func ExpandCodexPatch(base Op, patchText string) []Op {
	patchText = strings.TrimSpace(patchText)
	if !strings.Contains(patchText, "Begin Patch") {
		return nil
	}
	var ops []Op
	lines := strings.Split(patchText, "\n")
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(line, "*** Add File:"):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Add File:"))
			i++
			var body strings.Builder
			for i < len(lines) {
				l := lines[i]
				if strings.HasPrefix(strings.TrimSpace(l), "*** ") {
					break
				}
				if strings.HasPrefix(l, "+") {
					body.WriteString(strings.TrimPrefix(l, "+"))
					if i+1 < len(lines) {
						body.WriteByte('\n')
					}
				}
				i++
			}
			op := base
			op.Kind = KindWrite
			op.Path = path
			op.Body = []byte(strings.TrimSuffix(body.String(), "\n"))
			op.PatchText = ""
			ops = append(ops, op)
		case strings.HasPrefix(line, "*** Update File:"):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Update File:"))
			i++
			var oldB, newB strings.Builder
			mode := ""
			for i < len(lines) {
				l := lines[i]
				if strings.HasPrefix(strings.TrimSpace(l), "*** ") {
					break
				}
				if strings.HasPrefix(l, "-") {
					mode = "old"
					oldB.WriteString(strings.TrimPrefix(l, "-"))
					if i+1 < len(lines) {
						oldB.WriteByte('\n')
					}
				} else if strings.HasPrefix(l, "+") {
					mode = "new"
					newB.WriteString(strings.TrimPrefix(l, "+"))
					if i+1 < len(lines) {
						newB.WriteByte('\n')
					}
				} else if mode == "old" {
					oldB.WriteString(l)
					oldB.WriteByte('\n')
				} else if mode == "new" {
					newB.WriteString(l)
					newB.WriteByte('\n')
				}
				i++
			}
			op := base
			op.Kind = KindPatch
			op.Path = path
			op.OldString = strings.TrimSuffix(oldB.String(), "\n")
			op.NewString = strings.TrimSuffix(newB.String(), "\n")
			op.PatchText = ""
			ops = append(ops, op)
		case strings.HasPrefix(line, "*** Delete File:"):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File:"))
			op := base
			op.Kind = KindDelete
			op.Path = path
			op.PatchText = ""
			ops = append(ops, op)
			i++
		default:
			i++
		}
	}
	return ops
}
