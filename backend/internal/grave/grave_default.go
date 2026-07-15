//go:build !lakearch

package grave

import (
	"errors"

	"github.com/sxty9/prizm/graveyard"
)

// openLakearch is the no-op stand-in compiled when the `lakearch` build tag is absent, so
// the default build stays pure-Go. Set SCRAPR_GRAVEYARD=memory (or leave it empty) for dev.
func openLakearch(string) (graveyard.Graveyard, error) {
	return nil, errors.New("grave: scrapr was built without lakearch support (rebuild with -tags lakearch)")
}
