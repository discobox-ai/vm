#!/bin/bash
#---
# name: Go format
# type: file
# pattern: "**/*.go"
# notify_llm: false
#---

# Thin trigger: the rule lives in the Taskfile, so the hook and `task fmt`
# cannot drift.

set -euo pipefail

go tool task fmt
