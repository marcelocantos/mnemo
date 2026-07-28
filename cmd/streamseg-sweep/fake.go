// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/marcelocantos/mnemo/internal/streamseg"
)

// fakeSummariser answers drips deterministically, with no model calls.
//
// It exists so --dry-run can exercise the whole harness — grid, replay,
// scoring, aggregation — for free. That matters more than it sounds:
// without it, the first time the sweep machinery runs is also the first
// time it costs money, and a bug in the aggregation would be discovered
// only after paying for a grid.
//
// Its segmentation is deliberately naive (cut every fixed number of
// messages), so its Pk scores are meaningless as a result. They are a
// floor to compare a real model against, not a baseline to ship.
type fakeSummariser struct {
	period int
	seq    int
	open   string
	lastID int
}

func newFakeSummariser(p streamseg.SweepPoint) streamseg.Summariser {
	period := p.DripSize * 2
	if period < 4 {
		period = 4
	}
	return &fakeSummariser{period: period}
}

var msgIDRe = regexp.MustCompile(`(?m)^#(\d+) `)

func (f *fakeSummariser) Ask(_ context.Context, drip string) (string, error) {
	ids := msgIDRe.FindAllStringSubmatch(drip, -1)
	if len(ids) == 0 {
		return "", nil
	}
	var lines []string
	for _, m := range ids {
		id, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if f.open == "" {
			f.seq++
			f.open = fmt.Sprintf("t%d", f.seq)
			f.lastID = id
			lines = append(lines, fmt.Sprintf(
				`{"event":"open","span":%q,"from":%d,"label":"synthetic topic %d"}`, f.open, id, f.seq))
			continue
		}
		if id-f.lastID >= f.period {
			lines = append(lines, fmt.Sprintf(
				`{"event":"seal","span":%q,"to":%d,"summary":"synthetic span"}`, f.open, id))
			f.open = ""
		}
	}
	return strings.Join(lines, "\n"), nil
}

func (f *fakeSummariser) Restart(context.Context) error {
	f.open = ""
	return nil
}

func (f *fakeSummariser) Close() {}
