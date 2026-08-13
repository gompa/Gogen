package tui

import (
	"context"
	"strings"
	"sync"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// FocusTarget indicates which component has keyboard focus.
type FocusTarget int

const (
	FocusInput FocusTarget = iota
	FocusViewport
)

// maxPendingDeliveries caps queued system deliveries (job notices,
// subagent reports), mirroring the web delivery queue: overflow drops the
// OLDEST delivery — a stale job notice is worse than none.
const maxPendingDeliveries = 8

// ModalKind identifies the active modal overlay.
type ModalKind int

const (
	ModalNone ModalKind = iota
	ModalApproval
	ModalSessions
	ModalModels
	ModalHelp
	ModalCompletion
	ModalSubagents
)

// modelChangedMsg is sent by the TUI runner when a background agent state
// change (e.g. restore model validation) may have altered what the status
// bar shows. Handling it as a no-op still forces a re-render, which reads
// the live provider model.
type modelChangedMsg struct{}

// Model is the top-level Bubble Tea model for the TUI.
type Model struct {
	// Core references
	agent *agent.Agent
	cfg   *config.Config

	// Runtime
	ctx     context.Context
	program *tea.Program

	// Components
	viewport Viewport
	textarea textarea.Model

	// State
	focus        FocusTarget
	modal        ModalKind
	streamCancel context.CancelFunc // cancels the current streaming LLM call
	streaming    bool
	verbose      bool
	width        int
	height       int
	quitting     bool

	// Wait indicator (input area) while a turn is in flight
	spinner       spinner.Model
	progressPhase progressPhase
	progressLabel string

	// Chat content buffer (lines to render)
	chatLines        []string
	wrappedPrefix    string   // pre-wrapped content of chatLines[:len-1], ends with "\n" if non-empty
	wrappedContent   string   // pre-computed wrapped content for viewport
	maxWrappedWidth  int      // cached max line width (incremental, avoids O(N) scan)
	styledLines      []string // wrappedContent split by "\n" (cached for selection render)
	styledLinesDirty bool     // true when styledLines needs recomputation
	// Incremental wrapping state: prefixLines holds the viewport lines for
	// chatLines[:len-1] so streaming updates splice only the last line's wrap
	// instead of re-splitting the whole conversation. wrappedContentDirty
	// defers the wrappedPrefix+lastWrapped concatenation until a consumer
	// (selection rendering) actually needs the full string.
	prefixLines         []string
	lastWrapped         string
	wrappedContentDirty bool

	// Streaming accumulation
	streamAssistantBuf  strings.Builder
	streamAssistantLine int // chatLines index for the live assistant line (-1 if none)
	streamThinkingBuf   strings.Builder
	streamThinkingOpen  bool
	streamThinkingLine  int               // chatLines index for the open thinking line (-1 if none)
	streamToolCallNames map[int]string    // index -> name
	streamToolCallArgs  map[int]string    // index -> accumulated args deltas
	streamToolCallIDs   map[int]string    // index -> call ID (for correlating results)
	streamToolCallLines map[int]int       // index -> chatLines index where the tool call line was added
	toolCallDiffs       map[string]string // call ID -> diff text (for patch_file/show_diff)
	activeToolName      string            // tool being prepared or executed; names the progress indicator

	streamToolDiffCount map[int]int     // index -> number of diff lines already rendered progressively
	streamToolDiffStart map[int]int     // index -> chatLines index where the first diff line is (after top border)
	toolDiffShown       map[string]bool // call ID -> true if diff was shown progressively (skip in result)
	// Input history
	inputHistory []string
	historyIdx   int
	historyDraft string

	// Nested (subagent) sessions finished in this process (TUI children are
	// ephemeral; the /subagents modal lists them). Guarded because the
	// spawner records from the agent's turn goroutine while the modal
	// renders on the tea loop.
	subagentMu sync.Mutex
	subagents  []subagentRecord

	// Context stats
	contextStats agent.TurnContext
	contextLine  string
	// contextStreamBaseUsed/contextStreamEstAdded support a live, approximate
	// context-indicator update while a turn is streaming. Reading a.Messages
	// during streaming (via refreshContextStats) races with the streaming
	// goroutine mutating it, so instead we snapshot the last authoritative
	// Used count at stream start and add a cheap local estimate for tokens
	// arriving via already-safe (Update-thread) stream messages. The exact
	// count is restored via refreshContextStats() once streaming ends.
	contextStreamBaseUsed int
	contextStreamEstAdded int

	// Session
	sessionID string

	// Completion state
	completions    []string
	completionIdx  int
	completionLine string // the full line at time of tab press

	// Approval state
	approvalResult   chan bool
	approvalUI       *approvalUIState
	approvalInFlight bool

	// System message deliveries (job notices, subagent reports) queued by
	// program.Send(deliveryRequestMsg) from background goroutines; drained
	// on the Update thread only when no turn is streaming.
	pendingDeliveries []string
	// deliveryDrops counts deliveries dropped on queue overflow; rendered
	// as a system line at the next drain (never mid-stream).
	deliveryDrops int

	// Modal data
	sessionList   []agent.SessionInfo
	modelList     []llm.ModelInfo
	sessionCursor int
	modelCursor   int

	// Keymap
	keys KeyMap

	// Screen dimensions tracking
	ready bool

	// Text selection (mouse drag-to-select in viewport)
	selectionYOff     int // viewport YOffset at selection start (for stable coordinates)
	selection         *SelectionState
	wrappedLines      []string // ANSI-stripped wrapped content lines (coordinate mapping)
	wrappedLinesDirty bool
	// Render caches — recomputed only when inputs change.
	dividerCache       string
	dividerCacheWidth  int
	dividerCacheFocus  FocusTarget
	dividerCacheStream bool
	statusMsg          string // transient message (e.g. "Copied N chars")
}

// NewModel creates a new TUI model. Returns a pointer: Model contains a
// mutex (the subagent list guard), so values must not be copied.
func NewModel(a *agent.Agent, cfg *config.Config) *Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message or command..."
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.SetHeight(3)
	ta.MaxHeight = 8
	ta.CharLimit = 0 // no limit
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = InputPromptStyle
	ta.BlurredStyle.Prompt = InputPromptStyle
	ta.KeyMap.InsertNewline.SetEnabled(false) // we handle Enter ourselves

	// Remap textarea keybindings to match our CLI conventions
	ta.KeyMap.WordBackward = key.NewBinding(key.WithKeys("ctrl+left"))
	ta.KeyMap.WordForward = key.NewBinding(key.WithKeys("ctrl+right"))
	ta.KeyMap.DeleteWordBackward = key.NewBinding(key.WithKeys("ctrl+w"))
	ta.KeyMap.DeleteAfterCursor = key.NewBinding(key.WithKeys("ctrl+k"))
	ta.KeyMap.DeleteBeforeCursor = key.NewBinding(key.WithKeys("ctrl+u"))
	ta.KeyMap.LineStart = key.NewBinding(key.WithKeys("ctrl+a", "home"))
	ta.KeyMap.LineEnd = key.NewBinding(key.WithKeys("ctrl+e", "end"))
	ta.KeyMap.DeleteCharacterForward = key.NewBinding(key.WithKeys("ctrl+d", "delete"))
	ta.KeyMap.DeleteCharacterBackward = key.NewBinding(key.WithKeys("backspace"))

	vp := NewViewport(80, 20)
	// Scroll 1 line per wheel event (default is 3; lower for smoother
	// scrolling when the mouse/terminal sends multiple events per notch).
	vp.MouseWheelDelta = 1
	vp.Style = ViewportStyle

	verbose := cfg != nil && cfg.CLIVerbose

	modelLine := ""
	if a != nil {
		modelLine = a.CurrentModel()
	}

	m := Model{
		agent:               a,
		cfg:                 cfg,
		viewport:            vp,
		textarea:            ta,
		focus:               FocusInput,
		modal:               ModalNone,
		streaming:           false,
		verbose:             verbose,
		spinner:             newProgressSpinner(),
		progressPhase:       progressHidden,
		chatLines:           make([]string, 0, 64), // pre-allocate to reduce GC during streaming
		streamAssistantLine: -1,
		streamThinkingLine:  -1,
		streamToolCallNames: make(map[int]string),
		streamToolCallArgs:  make(map[int]string),
		streamToolCallIDs:   make(map[int]string),
		streamToolCallLines: make(map[int]int),
		toolCallDiffs:       make(map[string]string),
		streamToolDiffCount: make(map[int]int),
		streamToolDiffStart: make(map[int]int),
		toolDiffShown:       make(map[string]bool),
		keys:                DefaultKeyMap,
		sessionID:           "",
		approvalResult:      make(chan bool, 1),
		selectionYOff:       -1,
		selection:           nil,
		wrappedLines:        nil,
		statusMsg:           "",
	}

	if a != nil {
		m.sessionID = a.SessionID
		// Build initial history
		m.chatLines = renderMessages(a.Messages, a.WorkingDir, modelLine, a.Mode.String())
		m.setViewportContent()
		m.viewport.GotoBottom()
		if stats := a.ContextStats(context.Background()); stats.Snapshot.Limit > 0 || stats.Snapshot.Used > 0 {
			m.contextStats = stats
			m.contextLine = agent.FormatContextBrief(stats)
		}
		// Nested sessions run on this TUI's agent (feature-gated by the
		// subagent flag; the spawner itself is harmless when the tool is
		// not exposed).
		if cfg != nil && cfg.SubagentEnabled() {
			a.SetSubagentSpawner(&tuiSubagentSpawner{cfg: cfg, m: &m})
		}
	}

	return &m
}

// recordSubagent appends a finished nested session to the /subagents list
// (called from the spawner on the agent's turn goroutine).
func (m *Model) recordSubagent(r subagentRecord) {
	m.subagentMu.Lock()
	m.subagents = append(m.subagents, r)
	m.subagentMu.Unlock()
}

func (m *Model) SetSize(width, height int) {
	m.width = width
	m.height = height
	m.ready = true

	// Layout: status bar (1 line) at bottom, textarea above it, viewport fills rest
	statusBarHeight := 1
	textareaHeight := m.textarea.Height()
	if textareaHeight > 8 {
		textareaHeight = 8
	}
	if textareaHeight < 1 {
		textareaHeight = 1
	}

	vpHeight := height - statusBarHeight - textareaHeight - 1 // -1 for textarea border
	if vpHeight < 3 {
		vpHeight = 3
	}

	m.viewport.Width = width - 2 // padding
	m.viewport.Height = vpHeight
	m.textarea.SetWidth(width - 2)
	m.textarea.SetHeight(textareaHeight)

	m.setViewportContent()
	m.viewport.GotoBottom()
}
