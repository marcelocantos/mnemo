// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package store

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework Vision -framework CoreImage

#include "ocr_darwin.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// appleVisionAvailable is true on darwin — the Vision framework ships with
// the OS and is linked at build time. Non-darwin builds get a stub.
const appleVisionAvailable = true

// runAppleVisionOCRNative invokes the Vision framework in-process via CGO.
// No subprocess, no helper binary — direct function call.
func runAppleVisionOCRNative(imageBytes []byte) (string, *float64, error) {
	if len(imageBytes) == 0 {
		return "", nil, fmt.Errorf("apple_vision: empty image bytes")
	}

	result := C.mnemo_ocr_recognize(
		unsafe.Pointer(&imageBytes[0]),
		C.size_t(len(imageBytes)),
	)
	defer C.mnemo_ocr_free_result(&result)

	if result.error != nil {
		return "", nil, fmt.Errorf("apple_vision: %s", C.GoString(result.error))
	}

	var text string
	if result.text != nil {
		text = C.GoString(result.text)
	}
	confidence := float64(result.confidence)
	return text, &confidence, nil
}
