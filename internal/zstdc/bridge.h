// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

#ifndef MNEMO_ZSTD_BRIDGE_H
#define MNEMO_ZSTD_BRIDGE_H

#include <stdlib.h>

// mnemo_zstd_stats reports what a compression run actually did.
// workers_used is what libzstd granted, which may be fewer than asked
// for: multithreading is a compile-time option and a library built
// without it accepts the request and then runs single-threaded.
typedef struct {
    long long in_bytes;
    long long out_bytes;
    int workers_used;
} mnemo_zstd_stats;

// mnemo_zstd_compress_file compresses src into dst as a single zstd
// frame. Returns 0 on success; on failure returns non-zero and writes a
// message into err. Pass workers <= 0 for single-threaded.
int mnemo_zstd_compress_file(const char *src, const char *dst, int level,
                             int workers, mnemo_zstd_stats *st, char *err,
                             size_t errlen);

#endif
