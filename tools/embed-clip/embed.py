#!/usr/bin/env -S uv run --script
# Copyright 2026 Marcelo Cantos
# SPDX-License-Identifier: Apache-2.0
#
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "sentence-transformers>=3.0",
#   "torch",
#   "pillow",
# ]
# ///
"""
Image/text embedding helper for mnemo.

Reads a JSON request from stdin:
  {"mode": "image", "path": "/tmp/foo.png"}
  {"mode": "text", "text": "architecture diagram"}

Emits a JSON response on stdout:
  {"model": "...", "dim": 768, "vector": [0.1, 0.2, ...]}

On error, emits:
  {"error": "message"}
"""

import json
import sys


def main() -> None:
    try:
        req = json.load(sys.stdin)
    except Exception as e:
        json.dump({"error": f"failed to parse stdin JSON: {e}"}, sys.stdout)
        sys.stdout.write("\n")
        sys.exit(1)

    mode = req.get("mode", "")
    if mode not in ("image", "text"):
        json.dump({"error": f"invalid mode: {mode!r}; must be 'image' or 'text'"}, sys.stdout)
        sys.stdout.write("\n")
        sys.exit(1)

    # Lazy-import heavy deps after arg validation so errors surface fast.
    try:
        from sentence_transformers import SentenceTransformer
        from PIL import Image
    except ImportError as e:
        json.dump({"error": f"dependency not available: {e}"}, sys.stdout)
        sys.stdout.write("\n")
        sys.exit(1)

    model_name = "clip-ViT-B-32"  # ~340MB, widely available via sentence-transformers
    try:
        model = SentenceTransformer(model_name)
    except Exception as e:
        json.dump({"error": f"failed to load model {model_name!r}: {e}"}, sys.stdout)
        sys.stdout.write("\n")
        sys.exit(1)

    try:
        if mode == "image":
            path = req.get("path", "")
            if not path:
                json.dump({"error": "missing 'path' for image mode"}, sys.stdout)
                sys.stdout.write("\n")
                sys.exit(1)
            img = Image.open(path).convert("RGB")
            embedding = model.encode(img, convert_to_numpy=True)
        else:
            text = req.get("text", "")
            if not text:
                json.dump({"error": "missing 'text' for text mode"}, sys.stdout)
                sys.stdout.write("\n")
                sys.exit(1)
            embedding = model.encode(text, convert_to_numpy=True)
    except Exception as e:
        json.dump({"error": f"embedding failed: {e}"}, sys.stdout)
        sys.stdout.write("\n")
        sys.exit(1)

    vector = embedding.tolist()
    # Strip model path prefix if any, keep only basename.
    short_name = model_name.split("/")[-1]
    json.dump({"model": short_name, "dim": len(vector), "vector": vector}, sys.stdout)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
