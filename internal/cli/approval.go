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

		type result struct {
			line string
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			line, err := reader.ReadString('\n')
			ch <- result{line, err}
		}()

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case r := <-ch:
			if r.err != nil {
				return false, r.err
			}
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			answer := strings.TrimSpace(strings.ToLower(r.line))
			return answer == "y" || answer == "yes", nil
		}
	}
}
