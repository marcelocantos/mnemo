// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

#include "bridge.h"

#include <stdio.h>
#include <string.h>

// ZSTD_CCtx_getParameter is in zstd's static-linking-only surface. We
// compile the amalgamation into this package, so static linking is
// exactly what we are doing — and reading back the granted worker count
// is the point: it is the only way to detect a library built without
// multithreading, which otherwise accepts nbWorkers and runs on one core.
#define ZSTD_STATIC_LINKING_ONLY
#include "zstd.h"

// One function, one job: compress src to dst. Everything the caller
// needs to know comes back in *st; everything that can go wrong comes
// back as a message in err.
//
// The whole reason this is C rather than the pure-Go encoder is
// ZSTD_c_nbWorkers: libzstd splits the input into jobs, compresses them
// in parallel and stitches them into a SINGLE frame, using overlapLog to
// give each job preceding context. The Go encoder pipelines instead,
// which measured ~1.4x; framing chunks by hand in Go reaches full
// parallelism but produces a multi-frame stream that is ~4% larger.
//
// st->workers_used reports what libzstd actually granted, which is NOT
// always what was asked for: multithreading is a compile-time option, and
// a build without it silently accepts nbWorkers and then runs
// single-threaded. The caller checks the number rather than trusting it.
static int fail(char *err, size_t errlen, const char *msg) {
    if (err && errlen > 0) {
        snprintf(err, errlen, "%s", msg);
    }
    return 1;
}

static int failz(char *err, size_t errlen, const char *what, size_t code) {
    if (err && errlen > 0) {
        snprintf(err, errlen, "%s: %s", what, ZSTD_getErrorName(code));
    }
    return 1;
}

int mnemo_zstd_compress_file(const char *src, const char *dst, int level,
                             int workers, mnemo_zstd_stats *st, char *err,
                             size_t errlen) {
    FILE *fin = NULL;
    FILE *fout = NULL;
    ZSTD_CCtx *cctx = NULL;
    void *inbuf = NULL;
    void *outbuf = NULL;
    int rc = 1;

    if (st) {
        memset(st, 0, sizeof(*st));
    }

    fin = fopen(src, "rb");
    if (!fin) {
        return fail(err, errlen, "cannot open source file");
    }
    fout = fopen(dst, "wb");
    if (!fout) {
        fclose(fin);
        return fail(err, errlen, "cannot create destination file");
    }

    cctx = ZSTD_createCCtx();
    if (!cctx) {
        rc = fail(err, errlen, "ZSTD_createCCtx failed");
        goto done;
    }

    size_t code = ZSTD_CCtx_setParameter(cctx, ZSTD_c_compressionLevel, level);
    if (ZSTD_isError(code)) {
        rc = failz(err, errlen, "set compressionLevel", code);
        goto done;
    }
    // A checksum costs almost nothing and turns silent corruption into a
    // loud decompression failure — worth it for something whose only job
    // is to still be readable much later.
    code = ZSTD_CCtx_setParameter(cctx, ZSTD_c_checksumFlag, 1);
    if (ZSTD_isError(code)) {
        rc = failz(err, errlen, "set checksumFlag", code);
        goto done;
    }
    if (workers > 0) {
        code = ZSTD_CCtx_setParameter(cctx, ZSTD_c_nbWorkers, workers);
        if (ZSTD_isError(code)) {
            // A single-threaded build reports the failure here rather
            // than pretending. Either way the caller learns the truth
            // from workers_used below.
            rc = failz(err, errlen, "set nbWorkers", code);
            goto done;
        }
    }
    int granted = 0;
    code = ZSTD_CCtx_getParameter(cctx, ZSTD_c_nbWorkers, &granted);
    if (ZSTD_isError(code)) {
        rc = failz(err, errlen, "get nbWorkers", code);
        goto done;
    }

    size_t insize = ZSTD_CStreamInSize();
    size_t outsize = ZSTD_CStreamOutSize();
    inbuf = malloc(insize);
    outbuf = malloc(outsize);
    if (!inbuf || !outbuf) {
        rc = fail(err, errlen, "out of memory allocating stream buffers");
        goto done;
    }

    long long in_total = 0;
    long long out_total = 0;
    for (;;) {
        size_t read = fread(inbuf, 1, insize, fin);
        if (read == 0 && ferror(fin)) {
            rc = fail(err, errlen, "read error on source file");
            goto done;
        }
        in_total += (long long)read;
        int last = feof(fin) != 0;
        ZSTD_EndDirective mode = last ? ZSTD_e_end : ZSTD_e_continue;
        ZSTD_inBuffer in = {inbuf, read, 0};

        // Loop until zstd has consumed the input and, on the final
        // chunk, fully flushed the frame.
        for (;;) {
            ZSTD_outBuffer out = {outbuf, outsize, 0};
            size_t remaining = ZSTD_compressStream2(cctx, &out, &in, mode);
            if (ZSTD_isError(remaining)) {
                rc = failz(err, errlen, "compressStream2", remaining);
                goto done;
            }
            if (out.pos > 0) {
                if (fwrite(outbuf, 1, out.pos, fout) != out.pos) {
                    rc = fail(err, errlen, "write error on destination file");
                    goto done;
                }
                out_total += (long long)out.pos;
            }
            if (last) {
                if (remaining == 0) {
                    break;
                }
            } else if (in.pos == in.size) {
                break;
            }
        }
        if (last) {
            break;
        }
    }

    if (fflush(fout) != 0) {
        rc = fail(err, errlen, "flush failed on destination file");
        goto done;
    }

    if (st) {
        st->in_bytes = in_total;
        st->out_bytes = out_total;
        st->workers_used = granted;
    }
    rc = 0;

done:
    if (inbuf) free(inbuf);
    if (outbuf) free(outbuf);
    if (cctx) ZSTD_freeCCtx(cctx);
    if (fout) fclose(fout);
    if (fin) fclose(fin);
    return rc;
}
