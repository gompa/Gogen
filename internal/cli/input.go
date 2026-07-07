package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"gogen/internal/agent"

	"golang.org/x/term"
)

type lineCompleter func(line string) []string

var inputHistory []string

type keyKind int

const (
	keyRune keyKind = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyEnter
	keyBackspace
	keyDelete
	keyTab
	keyCtrlC
	keyCtrlD
	keyIgnore
	keyPasteStart
	keyPasteEnd
	keyWordLeft
	keyWordRight
	keyHome
	keyEnd
	keyCtrlA
	keyCtrlE
	keyCtrlK
	keyCtrlU
	keyCtrlW
)

func readByte(r io.Reader) (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r, b[:])
	return b[0], err
}

func parseKey(r io.Reader) (keyKind, rune, error) {
	b, err := readByte(r)
	if err != nil {
		return keyIgnore, 0, err
	}
	switch b {
	case '\r', '\n':
		return keyEnter, 0, nil
	case 1:
		return keyCtrlA, 0, nil
	case 3:
		return keyCtrlC, 0, nil
	case 4:
		return keyCtrlD, 0, nil
	case 5:
		return keyCtrlE, 0, nil
	case 11:
		return keyCtrlK, 0, nil
	case 21:
		return keyCtrlU, 0, nil
	case 23:
		return keyCtrlW, 0, nil
	case 127, 8:
		return keyBackspace, 0, nil
	case '\t':
		return keyTab, 0, nil
	case 27:
		b2, err := readByte(r)
		if err != nil {
			return keyIgnore, 0, err
		}
		var seq byte
		switch b2 {
		case '[':
			// Consume CSI parameter bytes until the final character
			// (0x40-0x7E). This handles plain arrows ([A), modified
			// arrows ([1;5D for Ctrl+Left), and bracketed paste
			// ([200~, [201~).
			var params []byte
			for {
				b, err := readByte(r)
				if err != nil {
					return keyIgnore, 0, err
				}
				params = append(params, b)
				if b >= 0x40 {
					break
				}
			}
			if string(params) == "200~" {
				return keyPasteStart, 0, nil
			}
			if string(params) == "201~" {
				return keyPasteEnd, 0, nil
			}
			// Delete key sends \x1b[3~
			if string(params) == "3~" {
				return keyDelete, 0, nil
			}
			// Home (old \x1b[1~) and End (old \x1b[4~)
			if string(params) == "1~" {
				return keyHome, 0, nil
			}
			if string(params) == "4~" {
				return keyEnd, 0, nil
			}
			last := params[len(params)-1]
			// Other ~ sequences (e.g. CSI <n>~ for n > 3): ignore.
			if last == '~' {
				return keyIgnore, 0, nil
			}
			// Modifier 5 = Ctrl. Handles both CSI formats:
			//   \x1b[1;5D  (xterm, most terminals)
			//   \x1b[5D    (Linux console, some terminals)
			raw := string(params[:len(params)-1])
			ctrl := false
			for _, p := range strings.Split(raw, ";") {
				if p == "5" {
					ctrl = true
					break
				}
			}
			switch last {
			case 'A':
				return keyUp, 0, nil
			case 'B':
				return keyDown, 0, nil
			case 'C':
				if ctrl {
					return keyWordRight, 0, nil
				}
				return keyRight, 0, nil
			case 'D':
				if ctrl {
					return keyWordLeft, 0, nil
				}
				return keyLeft, 0, nil
			case 'H':
				return keyHome, 0, nil
			case 'F':
				return keyEnd, 0, nil
			}
			return keyIgnore, 0, nil
		case 'O':
			seq, err = readByte(r)
		default:
			return keyIgnore, 0, nil
		}
		if err != nil {
			return keyIgnore, 0, err
		}
		switch seq {
		case 'A':
			return keyUp, 0, nil
		case 'B':
			return keyDown, 0, nil
		case 'C':
			return keyRight, 0, nil
		case 'D':
			return keyLeft, 0, nil
		}
		return keyIgnore, 0, nil
	default:
		if b >= 32 {
			return keyRune, rune(b), nil
		}
		return keyIgnore, 0, nil
	}
}


// wordLeft moves the cursor to the start of the previous word, matching
// bash's backward-word (Ctrl+Left) behavior.
func wordLeft(buf []rune, cursor int) int {
	if cursor == 0 {
		return 0
	}
	// Skip any non-alphanumeric characters immediately to the left
	// (whitespace, punctuation the cursor is sitting after).
	i := cursor
	for i > 0 && !isWordRune(buf[i-1]) {
		i--
	}
	// Skip alphanumeric characters.
	for i > 0 && isWordRune(buf[i-1]) {
		i--
	}
	return i
}

// wordRight moves the cursor to the start of the next word, matching
// bash's forward-word (Ctrl+Right) behavior.
func wordRight(buf []rune, cursor int) int {
	n := len(buf)
	if cursor >= n {
		return n
	}
	// Skip alphanumeric characters under / to the right of the cursor.
	i := cursor
	for i < n && isWordRune(buf[i]) {
		i++
	}
	// Skip non-alphanumeric characters.
	for i < n && !isWordRune(buf[i]) {
		i++
	}
	return i
}

func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '_'
}

func computeLayout(prompt string, buf []rune, cursor, cols int) (totalRows, curRow, curCol int) {
    comb := append([]rune(prompt), buf...)  // everything in runes
    pos := len([]rune(prompt)) + cursor  // rune length, not byte length

    r, c := 0, 0
    found := false

    for i, ch := range comb {
        if i == pos {
            curRow, curCol = r, c
            found = true
        }
        if ch == '\n' {
            r++
            c = 0
            continue
        }
        if cols > 0 && c >= cols {
            r++
            c = 0
        }
        c++
    }

    if !found {
        curRow, curCol = r, c
    }
    return r + 1, curRow, curCol
}


func readLine(prompt string, complete lineCompleter) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Print(prompt)
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	return readLineTTY(prompt, complete)
}

func readLineTTY(prompt string, complete lineCompleter) (string, error) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	// Enable bracketed paste mode so pasted newlines are not treated as Enter.
	fmt.Print("\x1b[?2004h")
	defer func() {
		fmt.Print("\x1b[?2004l")
		term.Restore(fd, oldState)
	}()

	linePrompt := strings.TrimLeft(prompt, "\n")
	if strings.HasPrefix(prompt, "\n") {
		fmt.Print("\n")
	}

	fmt.Print(linePrompt)
	// prevCurRow tracks the cursor row offset (0-based) from the prompt
	// line within the display.  prevTotalRows tracks the full display
	// height from the previous redraw so we can erase old content even
	// after the buffer shrinks.
	prevCurRow := 0
	prevTotalRows := 1

	var buf []rune
	cursor := 0
	historyIdx := len(inputHistory)
	draftLine := ""
	pasteMode := false

	redraw := func() {
		cols := terminalColumns()
		// Build display string with \x1b7 embedded at the logical cursor
		// position.  \x1b8 at the end restores the cursor to that spot
		// — zero manual column arithmetic, no \x1b[0C → 1 bugs.
		// \r\n substitution handles raw-mode ONLCR.
		promptRunes := []rune(linePrompt)
		cursorPos := len(promptRunes) + cursor
		fullRunes := append(promptRunes, buf...)
		var out strings.Builder
		out.Grow(len(linePrompt)*3 + len(buf)*2 + 12)
		for i, ch := range fullRunes {
			if i == cursorPos {
				out.WriteString("\x1b7")
			}
			if ch == '\n' {
				out.WriteString("\r\n")
			} else {
				out.WriteRune(ch)
			}
		}
		if cursorPos >= len(fullRunes) {
			out.WriteString("\x1b7")
		}
		display := out.String()

		// Compute layout for the current buffer content.  We need
		// the total rows the display will occupy so we can
		// pre-fill blank lines below the prompt, preventing the
		// terminal from scrolling stale (uncleared) content into
		// the input area.
		layoutCols := cols
		if layoutCols <= 0 {
			layoutCols = 1024
		}
		totalRows, curRow, _ := computeLayout(linePrompt, buf, cursor, layoutCols)

		if cols <= 0 {
			// Non-TTY fallback — clear the current line only.
			fmt.Print("\r\x1b[2K" + display + "\x1b8")
		} else {
			// Move to the prompt row.
			if prevCurRow > 0 {
				fmt.Printf("\x1b[%dA", prevCurRow)
			}
			// Clear from prompt to end of screen.
			fmt.Print("\r\x1b[J")

			// Print enough blank lines to cover the taller of
			// the previous display and the new display.  This
			// replaces old content and pre-positions the
			// terminal so the real display below never
			// scrolls in stale rows.
			needed := max(totalRows, prevTotalRows)
			for i := 1; i < needed; i++ {
				fmt.Print("\r\n")
			}
			// Move back up to the (now-clean) prompt row.
			if needed > 1 {
				fmt.Printf("\x1b[%dA", needed-1)
			}
			fmt.Print("\r\x1b[J" + display + "\x1b8")
		}

		prevCurRow = curRow
		prevTotalRows = totalRows
	}

	insert := func(r rune) {
		buf = append(buf[:cursor], append([]rune{r}, buf[cursor:]...)...)
		cursor++
	}

	insertRune := func(r rune) {
		insert(r)
		redraw()
	}

	deleteBeforeCursor := func() {
		if cursor > 0 {
			buf = append(buf[:cursor-1], buf[cursor:]...)
			cursor--
			redraw()
		}
	}

	deleteAfterCursor := func() {
		if cursor < len(buf) {
			buf = append(buf[:cursor], buf[cursor+1:]...)
			redraw()
		}
	}

	deleteWordBeforeCursor := func() {
		if cursor == 0 {
			return
		}
		// Whitespace-based backwards word deletion (readline Ctrl+W).
		i := cursor
		// Skip whitespace immediately left of cursor.
		for i > 0 && buf[i-1] == ' ' {
			i--
		}
		// Skip non-whitespace (the word).
		for i > 0 && buf[i-1] != ' ' {
			i--
		}
		buf = append(buf[:i], buf[cursor:]...)
		cursor = i
		redraw()
	}

	killToEnd := func() {
		if cursor < len(buf) {
			buf = buf[:cursor]
			redraw()
		}
	}

	killToStart := func() {
		if cursor > 0 {
			buf = buf[cursor:]
			cursor = 0
			redraw()
		}
	}

	setLine := func(line string) {
		buf = []rune(line)
		cursor = len(buf)
		redraw()
	}

	for {
		kind, ch, err := parseKey(os.Stdin)
		if err != nil {
			return "", err
		}
		switch kind {
		case keyPasteStart:
			pasteMode = true
		case keyPasteEnd:
			pasteMode = false
			redraw()
		case keyEnter:
			if pasteMode {
				insert('\n')
				continue
			}
			// Move cursor past the entire display so that
			// multiline (pasted) text scrolls up cleanly
			// rather than remaining on screen for agent
			// output to overwrite.
			remaining := prevTotalRows - prevCurRow - 1
			for i := 0; i < remaining; i++ {
				fmt.Print("\r\n")
			}
			fmt.Println()
			line := string(buf)
			if line != "" && (len(inputHistory) == 0 || inputHistory[len(inputHistory)-1] != line) {
				inputHistory = append(inputHistory, line)
			}
			return line, nil
		case keyCtrlC:
			fmt.Println()
			return "", io.EOF
		case keyCtrlD:
			if len(buf) == 0 {
				fmt.Println()
				return "", io.EOF
			}
			deleteAfterCursor()
		case keyBackspace:
			deleteBeforeCursor()
		case keyDelete:
			deleteAfterCursor()
		case keyHome, keyCtrlA:
			if cursor > 0 {
				cursor = 0
				redraw()
			}
		case keyEnd, keyCtrlE:
			if cursor < len(buf) {
				cursor = len(buf)
				redraw()
			}
		case keyCtrlW:
			deleteWordBeforeCursor()
		case keyCtrlK:
			killToEnd()
		case keyCtrlU:
			killToStart()
		case keyLeft, keyWordLeft:
			if cursor > 0 {
				if kind == keyWordLeft {
					cursor = wordLeft(buf, cursor)
				} else {
					cursor--
				}
				redraw()
			}
		case keyRight, keyWordRight:
			if cursor < len(buf) {
				if kind == keyWordRight {
					cursor = wordRight(buf, cursor)
				} else {
					cursor++
				}
				redraw()
			}
		case keyUp:
			if len(inputHistory) == 0 {
				continue
			}
			if historyIdx == len(inputHistory) {
				draftLine = string(buf)
			}
			if historyIdx > 0 {
				historyIdx--
				setLine(inputHistory[historyIdx])
			}
		case keyDown:
			if historyIdx >= len(inputHistory) {
				continue
			}
			historyIdx++
			if historyIdx == len(inputHistory) {
				setLine(draftLine)
			} else {
				setLine(inputHistory[historyIdx])
			}
		case keyTab:
			if complete == nil {
				continue
			}
			line := string(buf)
			matches := complete(line)
			if len(matches) == 0 {
				continue
			}
			prefix, arg, ok := agent.ResumeLinePrefix(line)
			if !ok {
				continue
			}
			if len(matches) == 1 {
				newArg := matches[0]
				if newArg == "del" {
					newArg = "del "
				}
				setLine(prefix + newArg)
				continue
			}
			cp := agent.LongestCommonPrefix(matches)
			if len(cp) > len(arg) {
				setLine(prefix + cp)
				continue
			}
			fmt.Print("\x1b7\n" + strings.Join(matches, "  ") + "\n\x1b8")
			redraw()
		case keyRune:
			if pasteMode {
				insert(ch)
			} else {
				insertRune(ch)
			}
		case keyIgnore:
		}
	}
}

func (c *CLI) completeLine(line string) []string {
	if prefix, arg, ok := agent.ResumeLinePrefix(line); ok {
		_ = prefix
		return c.agent.ResumeArgCompletions(arg)
	}
	return nil
}
