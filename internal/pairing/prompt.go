package pairing

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// CodeDigits mirrors the API. It is only used to tell somebody they typed the
// wrong number of digits before a round trip proves it.
const CodeDigits = 6

// ErrNoTerminal is returned when there is nobody to ask. An unattended install
// has to supply the code some other way, and saying so is more useful than
// hanging on a read that will never return.
var ErrNoTerminal = errors.New("no terminal to ask for the code")

// Normalize accepts what a person actually types. The dashboard shows the code
// grouped as "418 302", and a paste of that should work.
func Normalize(raw string) string {
	var digits strings.Builder
	for _, character := range raw {
		if character >= '0' && character <= '9' {
			digits.WriteRune(character)
		}
	}
	return digits.String()
}

// Ask reads the code from the terminal.
//
// The installer arrives through a pipe, so standard input holds the script
// rather than the keyboard. The terminal has to be opened directly; there is no
// way around it and no reason to guess.
func Ask(out io.Writer, dashboardURL string) (string, error) {
	terminal, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", ErrNoTerminal
	}
	defer terminal.Close()

	fmt.Fprintf(out, "Add a server at %s and it shows a %d digit code.\n", dashboardURL, CodeDigits)
	fmt.Fprint(terminal, "Enter the code: ")

	line, err := bufio.NewReader(terminal).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("cannot read the code: %w", err)
	}

	code := Normalize(line)
	if len(code) != CodeDigits {
		return "", fmt.Errorf("a code is %d digits; got %d", CodeDigits, len(code))
	}
	return code, nil
}
