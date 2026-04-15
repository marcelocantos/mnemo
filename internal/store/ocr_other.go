// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package store

import "fmt"

const appleVisionAvailable = false

func runAppleVisionOCRNative(imageBytes []byte) (string, *float64, error) {
	return "", nil, fmt.Errorf("apple_vision: only available on macOS")
}
