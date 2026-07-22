package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gogen/internal/llm"
)

func (a *Agent) CurrentModel() string {
	return a.Provider.ModelName()
}

func (a *Agent) requireModelSelected(ctx context.Context) error {
	if a.Provider.ModelName() != "" {
		return nil
	}
	if a.Context != nil {
		a.Context.EnsureContextLimit(ctx)
	}
	if a.Provider.ModelName() != "" {
		return nil
	}
	return fmt.Errorf("no model selected; use /models to list and choose a model")
}

func (a *Agent) ContextLimit() int {
	if a.Context == nil {
		return 0
	}
	return a.Context.ContextLimit()
}

func (a *Agent) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	models, err := a.Provider.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	current := a.Provider.ModelName()
	for i := range models {
		models[i].Current = models[i].ID == current
	}
	return models, nil
}

func (a *Agent) SelectModel(ctx context.Context, selector string) error {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return fmt.Errorf("model selector is required")
	}

	models, err := a.ListModels(ctx)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no models available from the endpoint")
	}

	var modelID string
	if n, err := strconv.Atoi(selector); err == nil {
		if n < 1 || n > len(models) {
			return fmt.Errorf("invalid model number %d (1-%d)", n, len(models))
		}
		modelID = models[n-1].ID
	} else {
		for _, m := range models {
			if m.ID == selector {
				modelID = m.ID
				break
			}
		}
		if modelID == "" {
			for _, m := range models {
				if strings.Contains(m.ID, selector) {
					modelID = m.ID
					break
				}
			}
		}
		if modelID == "" {
			return fmt.Errorf("model not found: %q", selector)
		}
	}

	if err := a.Provider.SetModel(modelID); err != nil {
		return err
	}
	if a.Context != nil {
		a.Context.RefreshAfterModelChange(ctx)
	}
	// A new model may report usage on a different scale (and context limit),
	// so the previous request's API counters are misleading for /context.
	a.lastTurnUsage = nil
	return nil
}

// ParseModelsCommand reports whether input is a /models command.
// If selectArg is non-empty, the user is selecting a model; otherwise it is list-only.
func ParseModelsCommand(input string) (selectArg string, ok bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed != "/models" && trimmed != "models" && !strings.HasPrefix(trimmed, "/models ") && !strings.HasPrefix(trimmed, "models ") {
		return "", false
	}
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "/models"), "models"))
	return arg, true
}

// HandleModelsCommand processes /models and /models <selector>.
// Returns output text and whether the command was handled.
func (a *Agent) HandleModelsCommand(ctx context.Context, input string) (string, bool, error) {
	arg, ok := ParseModelsCommand(input)
	if !ok {
		return "", false, nil
	}

	if arg != "" {
		if err := a.SelectModel(ctx, arg); err != nil {
			return "", true, err
		}
		limit := a.ContextLimit()
		if limit > 0 {
			return fmt.Sprintf("Switched to model: %s (context: %d tokens)", a.CurrentModel(), limit), true, nil
		}
		return fmt.Sprintf("Switched to model: %s", a.CurrentModel()), true, nil
	}

	models, err := a.ListModels(ctx)
	if err != nil {
		return "", true, err
	}
	if len(models) == 0 {
		return "No models reported by the endpoint.", true, nil
	}
	if len(models) == 1 {
		m := models[0]
		limit := m.ContextLimit
		if limit > 0 {
			return fmt.Sprintf("Single model available: %s (n_ctx=%d)", m.ID, limit), true, nil
		}
		return fmt.Sprintf("Single model available: %s", m.ID), true, nil
	}

	var b strings.Builder
	b.WriteString("Available models (* = current):\n")
	for i, m := range models {
		marker := " "
		if m.Current {
			marker = "*"
		}
		if m.ContextLimit > 0 {
			fmt.Fprintf(&b, "  %2d. %s  (n_ctx=%d) %s\n", i+1, m.ID, m.ContextLimit, marker)
		} else {
			fmt.Fprintf(&b, "  %2d. %s %s\n", i+1, m.ID, marker)
		}
	}
	b.WriteString("\nUse /models <number> or /models <name> to switch.")
	return b.String(), true, nil
}
