#!/bin/bash
#---
# name: Go mod tidy
# type: file
# pattern: "**/go.{mod,sum}"
# notify_llm: false
#---

set -euo pipefail

go tool task tidy
