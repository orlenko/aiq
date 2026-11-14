package diagnostic

func explicitLeaf() { panic("invented panic content") }

func boundsLeaf(i int) { _ = []int{1}[i] }

func nilLeaf() { var f func(); f() }

func nestedLeaf() { explicitLeaf() }
