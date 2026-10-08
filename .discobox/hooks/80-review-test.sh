#!/bin/bash
#---
# name: Tests
# type: file
# pattern: "**/*.go"
# phase: review
#---

set -euo pipefail

go tool task test
