#!/bin/bash
#---
# name: Dev build
# description: Rebuilds build/disco-vm whenever its source changes
#---

set -euo pipefail

exec go tool task dev
