#!/bin/bash
#---
# name: Static checks
# type: file
# pattern: "**/*.{go,mod,sum,yml,yaml,sh}"
# phase: review
#---

# golangci-lint, repocheck, shellcheck and actionlint, as `task check` defines
# them.

set -euo pipefail

go tool task check
