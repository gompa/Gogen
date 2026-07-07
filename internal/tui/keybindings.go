package tui

import "github.com/charmbracelet/bubbles/key"

// KeyMap defines all keybindings for the TUI.
type KeyMap struct {
	// Input
	Submit       key.Binding
	CancelTurn   key.Binding
	ForceQuit    key.Binding
	BackwardWord key.Binding
	ForwardWord  key.Binding
	KillToEnd    key.Binding
	KillToStart  key.Binding
	KillWord     key.Binding
	DeleteForward key.Binding
	DeleteBack   key.Binding
	LineStart    key.Binding
	LineEnd      key.Binding
	HistoryUp    key.Binding
	HistoryDown  key.Binding
	Completion   key.Binding

	// Viewport
	ViewportUp       key.Binding
	ViewportDown     key.Binding
	ViewportPageUp   key.Binding
	ViewportPageDown key.Binding
	ViewportTop      key.Binding
	ViewportBottom   key.Binding
	ViewportHalfUp   key.Binding
	ViewportHalfDown key.Binding
	FocusViewport    key.Binding
	FocusInput      key.Binding

	// Global
	Help    key.Binding
	Quit    key.Binding
	Verbose key.Binding
}

var DefaultKeyMap = KeyMap{
	Submit:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "submit")),
	CancelTurn:    key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "cancel turn / quit")),
	ForceQuit:     key.NewBinding(key.WithKeys("ctrl+\\"), key.WithHelp("ctrl+\\", "force quit")),
	BackwardWord:  key.NewBinding(key.WithKeys("ctrl+left"), key.WithHelp("ctrl+←", "word left")),
	ForwardWord:   key.NewBinding(key.WithKeys("ctrl+right"), key.WithHelp("ctrl+→", "word right")),
	KillToEnd:     key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("ctrl+k", "kill to end")),
	KillToStart:   key.NewBinding(key.WithKeys("ctrl+u"), key.WithHelp("ctrl+u", "kill to start")),
	KillWord:      key.NewBinding(key.WithKeys("ctrl+w"), key.WithHelp("ctrl+w", "kill word")),
	DeleteForward: key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl+d", "delete forward")),
	DeleteBack:    key.NewBinding(key.WithKeys("backspace"), key.WithHelp("backspace", "delete back")),
	LineStart:     key.NewBinding(key.WithKeys("ctrl+a", "home"), key.WithHelp("ctrl+a", "line start")),
	LineEnd:       key.NewBinding(key.WithKeys("ctrl+e", "end"), key.WithHelp("ctrl+e", "line end")),
	HistoryUp:     key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "history back")),
	HistoryDown:   key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "history forward")),
	Completion:    key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "complete")),

	ViewportUp:       key.NewBinding(key.WithKeys("ctrl+p"), key.WithHelp("ctrl+p", "scroll up")),
	ViewportDown:     key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("ctrl+n", "scroll down")),
	ViewportPageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "page up")),
	ViewportPageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdn", "page down")),
	ViewportTop:      key.NewBinding(key.WithKeys("home"), key.WithHelp("home", "top of history")),
	ViewportBottom:   key.NewBinding(key.WithKeys("end"), key.WithHelp("end", "bottom of history")),
	ViewportHalfUp:   key.NewBinding(key.WithKeys("ctrl+u"), key.WithHelp("ctrl+u", "half page up")),
	ViewportHalfDown: key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl+d", "half page down")),
	FocusViewport:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "focus viewport")),
	FocusInput:      key.NewBinding(key.WithKeys("i", "enter"), key.WithHelp("i/enter", "focus input")),

	Help:    key.NewBinding(key.WithKeys("f1"), key.WithHelp("f1 / ?", "help")),
	Quit:    key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	Verbose: key.NewBinding(key.WithKeys("ctrl+v"), key.WithHelp("ctrl+v", "toggle verbose")),
}

// FullHelp returns keybindings for the full help view.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Submit, k.CancelTurn, k.Quit, k.Help, k.Verbose},
		{k.BackwardWord, k.ForwardWord, k.KillToEnd, k.KillToStart, k.KillWord},
		{k.LineStart, k.LineEnd, k.DeleteForward, k.DeleteBack, k.Completion},
		{k.HistoryUp, k.HistoryDown},
		{k.ViewportPageUp, k.ViewportPageDown, k.ViewportTop, k.ViewportBottom},
		{k.FocusViewport, k.FocusInput},
	}
}

// ShortHelp returns keybindings for the status bar.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Help, k.Quit}
}
