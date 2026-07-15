//go:build lakearch

package grave

import (
	"github.com/sxty9/aigentic/graveyard/lakegrave"
	"github.com/sxty9/prizm/graveyard"
)

// openLakearch opens a lakearch-backed graveyard bound to dir (the store selector — a
// lakearch "Bestand" is one Kernel per directory). Reuses aigentic's existing cgo binding
// rather than re-vendoring the FFI; its relative #cgo paths resolve via the sibling-repo
// layout (/home/nanu/{scrapr,aigentic,prizm,lakearch}). dir defaults to the state dir.
func openLakearch(dir string) (graveyard.Graveyard, error) {
	if dir == "" {
		dir = "/var/lib/scrapr/lakearch"
	}
	return lakegrave.Open(dir)
}
