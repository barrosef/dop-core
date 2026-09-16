package contract

import (
	"fmt"

	"github.com/barrosef/dop-core/internal/domain/ports"
)

func fmtValue(v ports.SecretValue) string { return fmt.Sprintf("%v", v) }
