package contracts

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestBuildEngineMountsItsCachePolicyWhereTheEngineReadsIt(t *testing.T) {
	unit := string(read(t, filepath.Join(root(t), "ansible/roles/build_engine/templates/infra-dagger.service.j2")))
	for _, mount := range []string{"--volume /etc/infra-dagger/{{ build_engine.slug }}.toml:/etc/dagger/engine.toml:ro ", "--volume /etc/infra-dagger/engine.json:/etc/dagger/engine.json:ro "} {
		if !strings.Contains(unit, mount) {
			t.Fatalf("build engine unit does not mount %q, where the pinned engine reads its configuration:\n%s", mount, unit)
		}
	}
}

func TestBuildEngineReadinessRequiresAListeningSocket(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("requires python3")
	}
	var tasks []struct {
		Command        struct{ Argv []string } `yaml:"ansible.builtin.command"`
		Retries, Delay int
		Until          string
		CheckMode      *bool `yaml:"check_mode"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), "ansible/roles/build_engine/tasks/readiness.yml")), &tasks); err != nil {
		t.Fatal(err)
	}
	var arguments []string
	for _, task := range tasks {
		if len(task.Command.Argv) == 4 && task.Command.Argv[0] == "python3" {
			arguments = append([]string{}, task.Command.Argv[1:]...)
			if task.Retries*task.Delay < 180 || task.Until != "build_engine_ready.rc == 0" || task.CheckMode == nil || *task.CheckMode {
				t.Fatalf("engine startup is not awaited during apply and verification: %+v", task)
			}
		}
	}
	if len(arguments) == 0 {
		t.Fatal("build engine startup has no socket readiness probe")
	}
	directory, err := os.MkdirTemp("/tmp", "infra-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	socket := filepath.Join(directory, "engine.sock")
	arguments[len(arguments)-1] = socket
	probe := func() error { return exec.Command("python3", arguments...).Run() }
	if err := probe(); err == nil {
		t.Fatal("engine with no socket was ready")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	if err := probe(); err != nil {
		t.Fatalf("listening engine was not ready: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := probe(); err == nil {
		t.Fatal("stale engine socket was ready")
	}
}

func TestRunnerVerificationWaitsForEnginesBeforeProbingIsolation(t *testing.T) {
	var plays []struct {
		Tasks []struct {
			ImportRole struct {
				Name      string
				TasksFrom string `yaml:"tasks_from"`
			} `yaml:"ansible.builtin.import_role"`
			Shell string `yaml:"ansible.builtin.shell"`
		}
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), "ansible/verify-runners.yml")), &plays); err != nil {
		t.Fatal(err)
	}
	ready := false
	for _, play := range plays {
		for _, task := range play.Tasks {
			if task.ImportRole.Name == "build_engine" && task.ImportRole.TasksFrom == "readiness.yml" {
				ready = true
			}
			if strings.Contains(task.Shell, "own-engine=") {
				if !ready {
					t.Fatal("runner isolation is probed before build engines are ready")
				}
				return
			}
		}
	}
	t.Fatal("runner isolation probe missing")
}
