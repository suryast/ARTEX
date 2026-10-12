package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the "model-driven context compression" mechanism introduced in norma
// v0.4.0, exposed as a platform experimental feature toggled by the user in system
// settings. It is mutually exclusive with the built-in compaction: noaadapter.Enable
// is the only entry point, installing the context takeover manager (Compactor), the
// Compress tool, and three resident prompt segments in one call; not calling Enable
// means it is off (built-in compaction works as usual). The toggle is resolved by each
// agent's injected noaEnabledFn, read once per run, so a switch only affects runs
// started afterward, with no need to rebuild the agent.

// enableNoa wires noa into opts when the resolver reports it is on. archiveRoot is the
// persistence base directory for the compressed originals (the global workDir, so every
// agent lands uniformly under <workDir>/noa rather than being scattered across
// task/intent directories), and sessionID names the archive subdirectory beneath it
// (globally unique, so there is no collision within the same base directory).
//
// noa is an experimental feature: a wiring failure must not interrupt a real task. On
// error it is reported via onWarn and falls back to built-in compaction. On successful
// enablement it clears opts.Compaction to avoid agentcore warning about "two context
// managers set at once".
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa compression failed to enable, falling back to built-in compaction: " + err.Error())
		}
		return
	}
	// The Compactor overrides Compaction, but with both set agentcore warns every time; clear it explicitly.
	opts.Compaction = nil
}
