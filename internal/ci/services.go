package ci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/fredrir/infra/internal/process"
)

type Database struct {
	User      string
	Name      string
	Port      int
	Variables []string
	JDBC      bool
}

var databaseNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func WithDatabase(ctx context.Context, runner process.Runner, database Database, check func(context.Context, process.Runner) error) (err error) {
	if !databaseNamePattern.MatchString(database.User) || !databaseNamePattern.MatchString(database.Name) || database.Port < 0 || database.Port > 65535 {
		return errors.New("invalid test database")
	}
	database.Port, err = testServicePort(database.Port)
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "infra-postgres-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	var prefix []string
	if os.Geteuid() == 0 {
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
		if err := os.Chown(directory, uid, gid); err != nil {
			return err
		}
		prefix = []string{"runuser", "-u", "postgres", "--"}
	}
	return withDatabase(ctx, runner, database, directory, prefix, check)
}

func withDatabase(ctx context.Context, runner process.Runner, database Database, directory string, prefix []string, check func(context.Context, process.Runner) error) (err error) {
	postgres := func(ctx context.Context, name string, arguments ...string) error {
		if len(prefix) > 0 {
			return runner.Run(ctx, prefix[0], append(append([]string{}, prefix[1:]...), append([]string{name}, arguments...)...)...)
		}
		return runner.Run(ctx, name, arguments...)
	}
	if err := postgres(ctx, "initdb", "-D", directory, "-U", database.User, "--auth=trust", "--encoding=UTF8", "--locale=C"); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		err = errors.Join(err, postgres(cleanup, "pg_ctl", "-D", directory, "-m", "fast", "-w", "stop"))
	}()
	options := fmt.Sprintf("-h 127.0.0.1 -p %d -k '' -c jit=off -c fsync=off", database.Port)
	if err := postgres(ctx, "pg_ctl", "-D", directory, "-o", options, "-l", filepath.Join(directory, "server.log"), "-w", "start"); err != nil {
		if file, openErr := os.Open(filepath.Join(directory, "server.log")); openErr == nil {
			if runner.Stderr != nil {
				io.Copy(runner.Stderr, io.LimitReader(file, 65536))
			}
			file.Close()
		}
		return err
	}
	port := strconv.Itoa(database.Port)
	if database.Name != "postgres" {
		if err := runner.Run(ctx, "createdb", "-h", "127.0.0.1", "-p", port, "-U", database.User, database.Name); err != nil {
			return err
		}
	}
	address := fmt.Sprintf("postgresql://%s@127.0.0.1:%d/%s", database.User, database.Port, database.Name)
	if database.JDBC {
		address = fmt.Sprintf("jdbc:postgresql://127.0.0.1:%d/%s", database.Port, database.Name)
	}
	for _, variable := range database.Variables {
		if !regexpEnvironment.MatchString(variable) {
			return errors.New("invalid database environment variable")
		}
		runner.Env = append(runner.Env, variable+"="+address)
	}
	return check(ctx, runner)
}

func WithValkey(ctx context.Context, runner process.Runner, port int, check func(context.Context, process.Runner) error) (err error) {
	port, err = testServicePort(port)
	if err != nil {
		return err
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	finished := make(chan error, 1)
	go func() {
		finished <- runner.Run(serviceCtx, "valkey-server", "--bind", "127.0.0.1", "--port", strconv.Itoa(port), "--save", "", "--appendonly", "no")
	}()
	defer func() {
		select {
		case failure := <-finished:
			if ctx.Err() == nil {
				err = errors.Join(err, errors.New("Valkey exited during checks"), failure)
			}
		default:
			cancel()
			<-finished
		}
		cancel()
	}()
	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, connectErr := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(readyCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if connectErr == nil {
			connection.Close()
			break
		}
		select {
		case failure := <-finished:
			finished <- failure
			return errors.Join(errors.New("Valkey exited before readiness"), failure)
		case <-readyCtx.Done():
			return readyCtx.Err()
		case <-ticker.C:
		}
	}
	runner.Env = append(runner.Env, "CI_VALKEY_PORT="+strconv.Itoa(port))
	return errors.Join(check(ctx, runner), ctx.Err())
}

func testServicePort(port int) (int, error) {
	if port < 0 || port > 65535 {
		return 0, errors.New("invalid test service port")
	}
	if port > 0 {
		return port, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port = listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}

func PostgresPath() string {
	if path, err := exec.LookPath("initdb"); err == nil {
		return filepath.Dir(path)
	}
	paths, _ := filepath.Glob("/usr/lib/postgresql/*/bin/initdb")
	if len(paths) > 0 {
		return filepath.Dir(paths[len(paths)-1])
	}
	return ""
}
