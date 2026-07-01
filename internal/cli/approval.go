package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"gogen/internal/agent"
)

func deleteApprover() agent.DeleteApprover {
	reader := bufio.NewReader(os.Stdin)
	return func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		fmt.Printf("\nDelete approval required (%s):\n", req.Reason)
		for _, path := range req.Paths {
			fmt.Printf("  • %s\n", path)
		}
		fmt.Print("Allow delete? [y/N]: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		answer := strings.TrimSpace(strings.ToLower(line))
		return answer == "y" || answer == "yes", nil
	}
}
