package catalog

import "github.com/barrosef/dop-core/internal/platform/errs"

// IsNotFound is the one predicate the service needs from errs, named here so
// the test's fake and the adapter agree on what "absent" looks like.
func IsNotFound(err error) bool { return errs.KindOf(err) == errs.KindNotFound }
