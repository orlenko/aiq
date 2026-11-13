package transcript

import (
	"strings"
	"time"
)

func panicOriginLeafA() { panic("invented private leaf A") }

func panicOriginLeafB() time.Time { panic("invented private leaf B") }

func panicOriginExternalLeaf() { strings.Repeat("fixture", -1) }
