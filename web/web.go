// Package web holds the dashboard's templates and static assets, embedded so
// the radar binary can render the site from anywhere.
package web

import "embed"

//go:embed templates static
var FS embed.FS
