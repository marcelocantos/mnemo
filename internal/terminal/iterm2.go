// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"context"

	"github.com/marcelocantos/mnemo/internal/iterm"
)

// iterm2Backend drives iTerm2 via AppleScript (🎯T85.2).
type iterm2Backend struct{}

func (iterm2Backend) Go(ctx context.Context, args GoArgs) (Result, error) {
	res, err := iterm.Go(ctx, iterm.GoArgs{
		Path:     args.Path,
		Name:     args.Name,
		NoResume: args.NoResume,
		Command:  args.Command,
		TagKey:   args.TagKey,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Action: Action(res.Action), Path: res.Path}, nil
}
