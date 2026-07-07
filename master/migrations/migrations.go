// Package migrations embeds the SQL schema migrations (golang-migrate
// format). They are applied automatically on master startup.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
