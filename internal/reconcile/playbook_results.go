package reconcile

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type playbookTask struct {
	Playbook string
	Host     string
	Task     string
	Changed  bool
	Failed   bool
}

type playbookRun struct {
	Reported    bool
	Tasks       []playbookTask
	Unreachable []string
}

var (
	recapPattern    = regexp.MustCompile(`(?m)^(\S+)\s+:\s+ok=\d+\s+changed=\d+\s+unreachable=(\d+)\s`)
	escapePattern   = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	testCasePattern = regexp.MustCompile(`^\[([^\]]+)\] (.+)$`)
)

func (c *Commands) recordPlaybooks(ctx context.Context, playbooks []string, args ...string) (playbookRun, error) {
	reports, err := os.MkdirTemp(c.Work, "ansible-results-")
	if err != nil {
		return playbookRun{}, err
	}
	defer os.RemoveAll(reports)
	var output bytes.Buffer
	record := Commands{Runner: c.Runner}
	record.Runner.Env = append(slices.Clone(c.Runner.Env), "ANSIBLE_CALLBACKS_ENABLED=ansible.builtin.junit", "JUNIT_OUTPUT_DIR="+reports, "JUNIT_HIDE_TASK_ARGUMENTS=true")
	record.Runner.Stdout = &output
	if c.Runner.Stdout != nil {
		record.Runner.Stdout = io.MultiWriter(c.Runner.Stdout, &output)
	}
	run := record.ansible(ctx, playbooks[0], append(slices.Clone(playbooks[1:]), args...)...)
	result := playbookRun{Unreachable: unreachableHosts(output.String())}
	var reportErr error
	result.Reported, result.Tasks, reportErr = recordedTasks(reports)
	return result, errors.Join(run, reportErr)
}

func unreachableHosts(output string) []string {
	var hosts []string
	for _, match := range recapPattern.FindAllStringSubmatch(escapePattern.ReplaceAllString(output, ""), -1) {
		if match[2] != "0" {
			hosts = append(hosts, match[1])
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

func recordedTasks(reports string) (bool, []playbookTask, error) {
	files, err := filepath.Glob(filepath.Join(reports, "*.xml"))
	if err != nil {
		return false, nil, err
	}
	written := func(file string) float64 {
		name := strings.TrimSuffix(filepath.Base(file), ".xml")
		value, _ := strconv.ParseFloat(name[strings.LastIndex(name, "-")+1:], 64)
		return value
	}
	slices.SortFunc(files, func(a, b string) int { return cmp.Compare(written(a), written(b)) })
	seen := map[string]bool{}
	var tasks []playbookTask
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return false, nil, err
		}
		var report struct {
			Suites []struct {
				Name  string `xml:"name,attr"`
				Cases []struct {
					Name     string     `xml:"name,attr"`
					Class    string     `xml:"classname,attr"`
					Failures []xml.Name `xml:"failure"`
					Errors   []xml.Name `xml:"error"`
					Output   string     `xml:"system-out"`
				} `xml:"testcase"`
			} `xml:"testsuite"`
		}
		if err := xml.Unmarshal(data, &report); err != nil {
			return false, nil, fmt.Errorf("Ansible result report %s: %w", filepath.Base(file), err)
		}
		for _, suite := range report.Suites {
			for _, recorded := range suite.Cases {
				key := recorded.Name + "\x00" + recorded.Class
				match := testCasePattern.FindStringSubmatch(recorded.Name)
				if seen[key] || match == nil {
					continue
				}
				seen[key] = true
				var result struct{ Changed bool }
				_ = json.Unmarshal([]byte(recorded.Output), &result)
				task := playbookTask{Playbook: suite.Name, Host: match[1], Task: match[2], Changed: result.Changed, Failed: len(recorded.Failures) > 0 || len(recorded.Errors) > 0}
				if task.Changed || task.Failed {
					tasks = append(tasks, task)
				}
			}
		}
	}
	return len(files) > 0, tasks, nil
}

func (r playbookRun) outcome(differences Differences, failed []string, err error) error {
	var problems []error
	if len(differences) > 0 {
		problems = append(problems, differences)
	}
	if len(failed) > 0 {
		problems = append(problems, fmt.Errorf("host tasks failed: %s", strings.Join(failed, "; ")))
	}
	if len(r.Unreachable) > 0 {
		problems = append(problems, fmt.Errorf("unreachable hosts: %s", strings.Join(r.Unreachable, ", ")))
	}
	explained := len(r.Unreachable) > 0 || slices.ContainsFunc(r.Tasks, func(task playbookTask) bool { return task.Failed })
	if err != nil && !explained {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}
