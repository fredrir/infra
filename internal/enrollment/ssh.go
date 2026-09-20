package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func SSHTransport(binary string, sudo bool) (Transport, error) {
	return sshTransport(binary, sudo, process.Run)
}

func sshTransport(binary string, sudo bool, run func(context.Context, process.Options) (process.Result, error)) (Transport, error) {
	if len(binary) > 4096 || !regexp.MustCompile(`^/(?:[A-Za-z0-9_.+-]+/)*[A-Za-z0-9_.+-]+$`).MatchString(binary) || path.Clean(binary) != binary {
		return nil, fmt.Errorf("verified absolute remote binary path required")
	}
	return func(ctx context.Context, mode string, target Target, payload map[string]any) error {
		if err := ValidateTarget(target); err != nil {
			return err
		}
		if mode != "preflight" && mode != "deliver" && mode != "cleanup" {
			return fmt.Errorf("invalid runtime operation")
		}
		if target.KeyID != "" && !keyIDPattern.MatchString(target.KeyID) {
			return fmt.Errorf("invalid key identity")
		}
		command := []string{binary, "operations", "runtime-key", mode, "--key-file", target.KeyFile, "--node", target.Node, "--role", target.Role, "--host", target.Host}
		if target.KeyID != "" {
			command = append(command, "--key-id", target.KeyID)
		}
		if sudo {
			command = append([]string{"/usr/bin/sudo", "-n", "--"}, command...)
		}
		var environment []string
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if slices.Contains([]string{"PATH", "HOME", "USER", "LOGNAME", "SSH_AUTH_SOCK", "LANG", "LC_ALL"}, name) {
				environment = append(environment, entry)
			}
		}
		var data []byte
		if payload != nil {
			var err error
			data, err = json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("invalid runtime payload")
			}
		}
		arguments := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "ConnectTimeout=10", target.Host, strings.Join(command, " ")}
		result, err := run(ctx, process.Options{Name: "ssh", Args: arguments, Env: environment, Stdin: bytes.NewReader(data), Timeout: 30 * time.Second})
		if err != nil {
			return fmt.Errorf("SSH runtime key operation failed; output withheld")
		}
		var output map[string]string
		if json.Unmarshal(result.Stdout, &output) != nil || len(output) != 1 || output["result"] != "ok" {
			return fmt.Errorf("SSH runtime key operation failed; output withheld")
		}
		return nil
	}, nil
}
