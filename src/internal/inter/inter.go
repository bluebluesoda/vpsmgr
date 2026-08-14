package inter

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// IsTTY reports whether stdin is an interactive terminal.
func IsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

var rd *bufio.Reader

func readLine() (string, error) {
	if rd == nil {
		rd = bufio.NewReader(os.Stdin)
	}
	line, err := rd.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Confirm prints "label [y/N]: " (defTrue -> "[Y/n]: ") and returns true for
// y/yes. Empty input resolves to the default. Anything else re-prompts.
func Confirm(label string, defTrue bool) (bool, error) {
	suffix := " [y/N]"
	if defTrue {
		suffix = " [Y/n]"
	}
	for {
		fmt.Printf("%s%s: ", label, suffix)
		line, err := readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(line) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		case "":
			return defTrue, nil
		default:
			fmt.Println("  ! answer y or n")
		}
	}
}

// Ask prints "label [default] unit: " and returns either the validated input
// or the default on empty input / EOF. Invalid input re-prompts showing the
// validation error.
func Ask(label, def, unit string, validate func(string) error) (string, error) {
	for {
		defStr := ""
		if def != "" {
			defStr = " [" + def + "]"
		}
		fmt.Printf("%s%s%s: ", label, defStr, unit)
		line, err := readLine()
		if err != nil {
			return "", err
		}
		if line == "" {
			return def, nil
		}
		if err := validate(line); err != nil {
			fmt.Printf("  ! %v\n", err)
			continue
		}
		return line, nil
	}
}
