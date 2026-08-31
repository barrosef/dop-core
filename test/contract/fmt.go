package contract

import (
	"fmt"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

func fmtValue(v ports.SecretValue) string { return fmt.Sprintf("%v", v) }
