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

// noa is a model-driven context compaction mechanism introduced in norma v0.4.0
// and exposed as an experimental platform setting. It is mutually exclusive with
// built-in compaction: noaadapter.Enable is the single entry point and installs the
// context manager (Compactor), Compress tool, and three persistent prompts. Without
// Enable, built-in compaction works as usual. Each agent's noaEnabledFn reads the
// setting once per run, so changes affect only subsequent runs.

// enableNoa adds noa to opts when the setting is enabled. archiveRoot is the persistent
// base directory for source archives (the global workDir, shared by all agents under
// <workDir>/noa rather than scattered across task/intent directories); sessionID names
// the archive subdirectory and is globally unique.
//
// noa is experimental: failure to attach must not interrupt real work. Report errors
// through onWarn and fall back to built-in compaction. On success, clear opts.Compaction
// to prevent agentcore warnings about configuring two context managers.
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
			onWarn("Failed to enable noa compaction; falling back to built-in compaction: " + err.Error())
		}
		return
	}
	// Compactor replaces Compaction; agentcore warns if both are set, so clear it explicitly.
	opts.Compaction = nil
}
