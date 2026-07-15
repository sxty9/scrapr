// Package grave selects the graveyard substrate (G) for the scrapr daemon: the in-memory
// stub by default, or lakearch when built with -tags lakearch. Keeping the lakearch (cgo)
// backend behind a build tag means the default build stays pure-Go and needs neither a C
// toolchain nor the lakearch library present — so dev, `go test` and the Wikipedia loop all
// run without cargo. Production builds `-tags lakearch` to write artifact bytes into the
// content-addressed store. This mirrors aigentic/backend/internal/grave.
package grave

import (
	"fmt"

	"github.com/sxty9/prizm/graveyard"
)

// Open returns a graveyard for the named backend. "memory" (the default) is the in-memory
// stub; "lakearch" is the content-addressed, append-only substrate (needs the `lakearch`
// build tag and a writable dir). dir is the store directory; for lakearch it defaults to
// /var/lib/scrapr/lakearch when empty.
func Open(kind, dir string) (graveyard.Graveyard, error) {
	switch kind {
	case "", "memory":
		return graveyard.NewMemory(), nil
	case "lakearch":
		return openLakearch(dir)
	default:
		return nil, fmt.Errorf("grave: unknown backend %q (want memory|lakearch)", kind)
	}
}
