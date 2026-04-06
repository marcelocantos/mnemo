#!/usr/bin/env python3
# Copyright 2026 Marcelo Cantos
# SPDX-License-Identifier: Apache-2.0

"""Scan all Claude Code transcript JSONL files and build a comprehensive
field census — every JSON path down to leaves, with histograms, type
distributions, size metrics, and value samples.

Usage:
    python3 scripts/field-census.py [--top N] [--min-count N] [--output FILE]

Output is a table sorted by frequency, with columns:
    path, count, types, null%, size deciles (0,10,20,...,100),
    distinct count (if < 1000), top values (if categorical)
"""

import argparse
import json
import math
import os
import statistics
import sys
from collections import Counter, defaultdict
from pathlib import Path


def walk_json(obj, path="$"):
    """Yield (path, value) for every leaf in a JSON object."""
    if isinstance(obj, dict):
        if not obj:
            yield path, obj  # empty dict is a leaf
        for k, v in obj.items():
            yield from walk_json(v, f"{path}.{k}")
    elif isinstance(obj, list):
        if not obj:
            yield path, obj  # empty list is a leaf
        for i, v in enumerate(obj):
            yield from walk_json(v, f"{path}[*]")
    else:
        yield path, value_to_leaf(obj)


def value_to_leaf(v):
    return v


def type_name(v):
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "bool"
    if isinstance(v, int):
        return "int"
    if isinstance(v, float):
        return "float"
    if isinstance(v, str):
        return "str"
    if isinstance(v, list):
        return "list(empty)"
    if isinstance(v, dict):
        return "dict(empty)"
    return type(v).__name__


def value_size(v):
    """Return a size metric for a value (bytes for strings, magnitude for numbers)."""
    if v is None:
        return 0
    if isinstance(v, bool):
        return 1
    if isinstance(v, (int, float)):
        return abs(v) if v != 0 else 0
    if isinstance(v, str):
        return len(v)
    if isinstance(v, (list, dict)):
        return 0  # empty containers
    return 0


def deciles(values):
    """Return all 11 deciles (0th, 10th, 20th, ..., 100th percentile)."""
    if not values:
        return [0] * 11
    s = sorted(values)
    n = len(s)
    result = []
    for p in range(0, 101, 10):
        idx = (p / 100) * (n - 1)
        lo = int(math.floor(idx))
        hi = int(math.ceil(idx))
        frac = idx - lo
        if lo == hi:
            result.append(s[lo])
        else:
            result.append(s[lo] * (1 - frac) + s[hi] * frac)
    return result


def format_size(v):
    """Format a size value compactly."""
    if isinstance(v, float):
        if v == int(v):
            return str(int(v))
        return f"{v:.1f}"
    return str(v)


def main():
    parser = argparse.ArgumentParser(description="Census of JSONL transcript fields")
    parser.add_argument("--top", type=int, default=0, help="Show only top N paths (0=all)")
    parser.add_argument("--min-count", type=int, default=0, help="Hide paths with fewer occurrences")
    parser.add_argument("--output", type=str, default="", help="Write output to file (default: stdout)")
    parser.add_argument("--projects-dir", type=str,
                        default=os.path.expanduser("~/.claude/projects"),
                        help="Path to Claude projects directory")
    args = parser.parse_args()

    projects_dir = Path(args.projects_dir)
    if not projects_dir.is_dir():
        print(f"error: {projects_dir} is not a directory", file=sys.stderr)
        sys.exit(1)

    # Collect stats per path.
    # For each path: count, type counter, sizes list, value counter (for categoricals).
    path_count = Counter()
    path_types = defaultdict(Counter)
    path_sizes = defaultdict(list)
    path_values = defaultdict(Counter)  # only track if values look categorical
    path_null_count = defaultdict(int)

    # Track total entries for context.
    total_entries = 0
    total_files = 0
    total_bytes = 0
    errors = 0

    # Find all JSONL files.
    jsonl_files = sorted(projects_dir.rglob("*.jsonl"))
    n_files = len(jsonl_files)

    print(f"Scanning {n_files} JSONL files...", file=sys.stderr)

    for file_idx, jsonl_path in enumerate(jsonl_files):
        if (file_idx + 1) % 500 == 0 or file_idx == 0:
            print(f"  [{file_idx+1}/{n_files}] {jsonl_path.name}...", file=sys.stderr)

        total_files += 1
        try:
            file_size = jsonl_path.stat().st_size
            total_bytes += file_size
        except OSError:
            continue

        try:
            with open(jsonl_path, "r", encoding="utf-8", errors="replace") as f:
                for line_no, line in enumerate(f, 1):
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        obj = json.loads(line)
                    except json.JSONDecodeError:
                        errors += 1
                        continue

                    total_entries += 1

                    for path, value in walk_json(obj):
                        path_count[path] += 1
                        t = type_name(value)
                        path_types[path][t] += 1
                        sz = value_size(value)
                        # Keep sizes list bounded — reservoir sample if too large.
                        sizes = path_sizes[path]
                        if len(sizes) < 100_000:
                            sizes.append(sz)

                        if value is None:
                            path_null_count[path] += 1

                        # Track values for potential categoricals.
                        # Only track if string/int/bool and value is short.
                        if isinstance(value, (bool, int)) or (
                            isinstance(value, str) and len(value) <= 100
                        ):
                            vc = path_values[path]
                            if len(vc) < 1000:  # stop tracking if too many distinct
                                vc[value] += 1
                            elif value in vc:
                                vc[value] += 1

        except Exception as e:
            print(f"  error reading {jsonl_path}: {e}", file=sys.stderr)
            errors += 1

    print(f"\nDone. {total_entries:,} entries from {total_files:,} files "
          f"({total_bytes/1024/1024:.0f} MB), {errors} errors.", file=sys.stderr)

    # Build output.
    out = sys.stdout
    if args.output:
        out = open(args.output, "w")

    # Sort by count descending.
    paths = sorted(path_count.keys(), key=lambda p: (-path_count[p], p))

    if args.min_count > 0:
        paths = [p for p in paths if path_count[p] >= args.min_count]
    if args.top > 0:
        paths = paths[:args.top]

    # Summary header.
    print(f"# Claude Code Transcript Field Census", file=out)
    print(f"#", file=out)
    print(f"# {total_entries:,} JSONL entries from {total_files:,} files "
          f"({total_bytes/1024/1024:.0f} MB)", file=out)
    if errors:
        print(f"# {errors} parse errors", file=out)
    print(f"# {len(path_count)} distinct paths", file=out)
    print(f"", file=out)

    # Table header.
    header = (
        f"{'path':<65s}  "
        f"{'count':>8s}  "
        f"{'%':>5s}  "
        f"{'types':<20s}  "
        f"{'null%':>5s}  "
        f"{'size p0':>7s}  "
        f"{'p10':>7s}  "
        f"{'p20':>7s}  "
        f"{'p30':>7s}  "
        f"{'p40':>7s}  "
        f"{'p50':>7s}  "
        f"{'p60':>7s}  "
        f"{'p70':>7s}  "
        f"{'p80':>7s}  "
        f"{'p90':>7s}  "
        f"{'p100':>7s}  "
        f"{'distinct':>8s}  "
        f"top_values"
    )
    print(header, file=out)
    print("-" * len(header) + "-" * 40, file=out)

    for path in paths:
        count = path_count[path]
        pct = 100.0 * count / total_entries if total_entries > 0 else 0

        # Types.
        tc = path_types[path]
        types_str = ",".join(f"{t}:{n}" for t, n in tc.most_common(4))

        # Null percentage.
        null_pct = 100.0 * path_null_count[path] / count if count > 0 else 0

        # Size deciles.
        sizes = path_sizes[path]
        d = deciles(sizes)

        # Distinct values / top values.
        vc = path_values[path]
        if vc and len(vc) < 500:
            distinct = len(vc)
            top = vc.most_common(5)
            top_str = "; ".join(f"{json.dumps(v)}({n})" for v, n in top)
        elif vc:
            distinct = len(vc)
            top_str = f"({distinct}+ distinct)"
        else:
            distinct = -1
            top_str = ""

        distinct_str = str(distinct) if distinct >= 0 else "-"

        print(
            f"{path:<65s}  "
            f"{count:>8d}  "
            f"{pct:>5.1f}  "
            f"{types_str:<20s}  "
            f"{null_pct:>5.1f}  "
            + "  ".join(f"{format_size(v):>7s}" for v in d)
            + f"  {distinct_str:>8s}  "
            + top_str,
            file=out,
        )

    if args.output:
        out.close()
        print(f"Written to {args.output}", file=sys.stderr)


if __name__ == "__main__":
    main()
