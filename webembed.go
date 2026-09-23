package main

import "embed"

// webFS embeds the web/ client directory into the binary so the server serves
// the UI (ui-spec U1: same origin as the API, relative URLs only) from ANY
// working directory — not just the repository root. fs.Sub(webFS, "web") in
// main yields the client root handed to the HTTP layer; no new dependencies.
//
//go:embed web
var webFS embed.FS
