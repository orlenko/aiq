package transcript

import (
	"strings"
	"time"
)

func viewOriginLeafA() { panic("invented private View leaf A") }

func viewOriginClockB() time.Time { panic("invented private View leaf B") }

func viewOriginExternal() { strings.Repeat("fixture", -1) }
