package cli

import (
	"os"
	"strconv"
)

func terminalColumns() int {
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if n, err := strconv.Atoi(cols); err == nil && n > 0 {
			return n
		}
	}
	if w, ok := terminalSize(int(os.Stdout.Fd())); ok && w > 0 {
		return w
	}
	return 0
}
