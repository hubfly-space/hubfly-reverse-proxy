#!/usr/bin/env bash
set -euo pipefail

INPUT_DIR="${1:-.}"
OUTPUT_DIR="${2:-.}"

exec go run ./cmd/migrate-json-to-sqlite -input-dir "${INPUT_DIR}" -output-dir "${OUTPUT_DIR}"
