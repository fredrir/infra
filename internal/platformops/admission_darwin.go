package platformops

import (
	"context"
	"fmt"
	"io"
	"net"
)

func ServeRunnerAdmissions(context.Context, []*net.UnixListener, string, int, io.Writer) error {
	return fmt.Errorf("runner admission requires Linux")
}
