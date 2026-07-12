package server

// Version is the home-datacenter-api build version. Bump this when
// cutting a release — it's advertised in GET /api/v1/server/info
// and persisted to the server_identity row on first boot / upgrade.
//
// Convention: semver-ish, "MAJOR.MINOR.PATCH". The "v" prefix is
// intentionally omitted so the value parses cleanly as a version
// string in scripts and dashboards.
const Version = "0.10.0"
