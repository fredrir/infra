package ci

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func StartPostgres(ctx context.Context, runner Runner, temporary, variable string) error {
	if !regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`).MatchString(variable) || temporary == "" {
		return fmt.Errorf("PostgreSQL needs a temporary directory and environment variable name")
	}
	account, err := user.Lookup("postgres")
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp("/tmp", "postgres-")
	if err != nil {
		return err
	}
	started := false
	initialized := false
	defer func() {
		if !started {
			if initialized {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				_ = runner.Run(cleanup, "setpriv", "--reuid=postgres", "--regid=postgres", "--init-groups", "--", "/usr/lib/postgresql/16/bin/pg_ctl", "--pgdata="+filepath.Join(directory, "cluster"), "--wait", "--mode=fast", "stop")
			}
			os.RemoveAll(directory)
		}
	}()
	if err := os.Chown(directory, uid, gid); err != nil {
		return err
	}
	identity := []string{"--reuid=postgres", "--regid=postgres", "--init-groups", "--"}
	if err := runner.Run(ctx, "setpriv", append(identity, "/usr/lib/postgresql/16/bin/initdb", "--pgdata="+filepath.Join(directory, "cluster"), "--username=postgres", "--auth=trust")...); err != nil {
		return err
	}
	initialized = true
	if err := runner.Run(ctx, "setpriv", append(identity, "/usr/lib/postgresql/16/bin/pg_ctl", "--pgdata="+filepath.Join(directory, "cluster"), "--wait", "--log="+filepath.Join(directory, "server.log"), "--options=-c listen_addresses=127.0.0.1 -c port=5432 -c unix_socket_directories="+directory+" -c fsync=off", "start")...); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temporary, "postgres-directory"), []byte(directory), 0600); err != nil {
		return err
	}
	if err := AppendEnvironment(os.Getenv("GITHUB_ENV"), variable, "postgres://postgres@127.0.0.1:5432/postgres"); err != nil {
		_ = os.Remove(filepath.Join(temporary, "postgres-directory"))
		return err
	}
	started = true
	return nil
}

func StopPostgres(ctx context.Context, runner Runner, temporary string) error {
	if temporary == "" {
		return fmt.Errorf("runner temporary directory is required")
	}
	state := filepath.Join(temporary, "postgres-directory")
	data, err := os.ReadFile(state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	directory := string(data)
	if filepath.Dir(directory) != "/tmp" || !strings.HasPrefix(filepath.Base(directory), "postgres-") || strings.ContainsAny(directory, "\r\n\x00") {
		return fmt.Errorf("invalid PostgreSQL data directory")
	}
	if err := runner.Run(ctx, "setpriv", "--reuid=postgres", "--regid=postgres", "--init-groups", "--", "/usr/lib/postgresql/16/bin/pg_ctl", "--pgdata="+filepath.Join(directory, "cluster"), "--wait", "--mode=fast", "stop"); err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return os.Remove(state)
}
