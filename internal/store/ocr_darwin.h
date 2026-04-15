// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

#ifndef MNEMO_OCR_DARWIN_H
#define MNEMO_OCR_DARWIN_H

#include <stddef.h>

// MnemoOCRResult holds the output of a Vision text-recognition pass.
// On success, text is a UTF-8 null-terminated string (possibly empty) and
// error is NULL. On failure, text is NULL and error is a UTF-8
// null-terminated error message. Either way, the caller must free the
// result with mnemo_ocr_free_result.
typedef struct {
    char *text;
    double confidence; // mean of top-candidate confidences; 0 if no observations
    char *error;
} MnemoOCRResult;

// Recognise text in the given image data (any format CIImage accepts:
// PNG, JPEG, TIFF, HEIC, etc). Synchronous.
MnemoOCRResult mnemo_ocr_recognize(const void *data, size_t len);

// Free the strings inside r. Safe to call with NULL pointers inside r.
void mnemo_ocr_free_result(MnemoOCRResult *r);

#endif // MNEMO_OCR_DARWIN_H
