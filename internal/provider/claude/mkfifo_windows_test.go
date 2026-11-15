package claude

import "errors"

func syscallMkfifo(string) error { return errors.New("no FIFOs on Windows") }
