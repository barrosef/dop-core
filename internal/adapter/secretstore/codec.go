package secretstore

import (
	"encoding/base64"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

func b64(v ports.SecretValue) string { return base64.StdEncoding.EncodeToString(v) }

func unb64(s string) (ports.SecretValue, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "secret with invalid encoding")
	}
	return ports.SecretValue(raw), nil
}
