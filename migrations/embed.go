// Package migrations embeds the SQL schema so the gateway can self-migrate.
package migrations

import "embed"

// FS holds every *.sql migration, applied in lexical order.
//
//go:embed *.sql
var FS embed.FS
