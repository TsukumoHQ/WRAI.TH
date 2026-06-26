package main

import "embed"

// hookScripts embeds the canonical activity/identity hook scripts (the same
// skill/hooks/ that install.sh ships) so `agent-relay hooks install` is
// self-contained and version-matched — no network, no drift from a second copy.
//
//go:embed skill/hooks/*.sh
var hookScripts embed.FS
