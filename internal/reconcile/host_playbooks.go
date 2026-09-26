package reconcile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	convergencePlaybook = "reconcile.yml"
	factsPlaybook       = "facts.yml"
	runnerPlaybook      = "build-runners.yml"
	monitorPlaybook     = "external.yml"
)

var (
	roleNamePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	playbookNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.yml$`)
)

type hostPlaybookGraph struct {
	order []string
	roles map[string]map[string]bool
}

func loadHostPlaybookGraph(root string) (hostPlaybookGraph, error) {
	ansible := filepath.Join(root, "ansible")
	leaves, err := importedPlaybooks(ansible, convergencePlaybook, map[string]bool{})
	if err != nil {
		return hostPlaybookGraph{}, err
	}
	if len(leaves) == 0 || leaves[0] != factsPlaybook || slices.Contains(leaves[1:], factsPlaybook) {
		return hostPlaybookGraph{}, fmt.Errorf("%s must import %s first and once", convergencePlaybook, factsPlaybook)
	}
	graph := hostPlaybookGraph{order: slices.Concat(leaves[1:], []string{monitorPlaybook, volatilePlaybook}), roles: map[string]map[string]bool{}}
	for _, playbook := range graph.order {
		if graph.roles[playbook] != nil {
			return hostPlaybookGraph{}, fmt.Errorf("%s is converged more than once", playbook)
		}
		if graph.roles[playbook], err = playbookRoles(ansible, playbook); err != nil {
			return hostPlaybookGraph{}, err
		}
	}
	return graph, nil
}

func (g hostPlaybookGraph) users(path string) []string {
	var users []string
	if role, ok := strings.CutPrefix(path, "ansible/roles/"); ok {
		role, _, _ = strings.Cut(role, "/")
		for _, playbook := range g.order {
			if g.roles[playbook][role] {
				users = append(users, playbook)
			}
		}
		return users
	}
	if playbook, ok := strings.CutPrefix(path, "ansible/"); ok && slices.Contains(g.order, playbook) {
		return []string{playbook}
	}
	return nil
}

// scopedHostPlaybooks returns nil when any path requires every host playbook.
func scopedHostPlaybooks(root string, paths []string) ([]string, error) {
	graph, err := loadHostPlaybookGraph(root)
	if err != nil {
		return nil, err
	}
	selected := map[string]bool{}
	for _, path := range paths {
		single := Affected([]string{path})
		switch effectiveHostScope(single) {
		case HostScopeNone:
		case HostScopeMonitor:
			selected[monitorPlaybook] = true
		case HostScopeRunners:
			selected[runnerPlaybook] = true
			selected[monitorPlaybook] = selected[monitorPlaybook] || monitorCLIChanged(single)
		default:
			users := graph.users(path)
			if len(users) == 0 {
				return nil, nil
			}
			for _, playbook := range users {
				selected[playbook] = true
			}
		}
	}
	var playbooks []string
	for _, playbook := range graph.order {
		if selected[playbook] {
			playbooks = append(playbooks, playbook)
		}
	}
	return playbooks, nil
}

func loadPlaybook(ansible, file string) ([]map[string]any, error) {
	if !playbookNamePattern.MatchString(file) {
		return nil, fmt.Errorf("unsupported playbook reference %q", file)
	}
	data, err := os.ReadFile(filepath.Join(ansible, file))
	if err != nil {
		return nil, err
	}
	var plays []map[string]any
	if err := yaml.Unmarshal(data, &plays); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return plays, nil
}

func importedPlaybooks(ansible, file string, visiting map[string]bool) ([]string, error) {
	if visiting[file] {
		return nil, fmt.Errorf("%s imports itself", file)
	}
	visiting[file] = true
	defer delete(visiting, file)
	plays, err := loadPlaybook(ansible, file)
	if err != nil {
		return nil, err
	}
	var leaves []string
	imports := 0
	for _, play := range plays {
		imported, ok := play["import_playbook"]
		if !ok {
			continue
		}
		name, ok := imported.(string)
		if !ok || len(play) != 1 {
			return nil, fmt.Errorf("%s: unsupported import %v", file, play)
		}
		nested, err := importedPlaybooks(ansible, name, visiting)
		if err != nil {
			return nil, err
		}
		leaves = append(leaves, nested...)
		imports++
	}
	switch imports {
	case 0:
		return []string{file}, nil
	case len(plays):
		return leaves, nil
	default:
		return nil, fmt.Errorf("%s mixes plays and imports", file)
	}
}

func playbookRoles(ansible, file string) (map[string]bool, error) {
	plays, err := loadPlaybook(ansible, file)
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, play := range plays {
		if _, ok := play["import_playbook"]; ok {
			return nil, fmt.Errorf("%s: converged playbooks cannot import playbooks", file)
		}
		entries, ok := play["roles"].([]any)
		if !ok && play["roles"] != nil {
			return nil, fmt.Errorf("%s: unsupported roles %v", file, play["roles"])
		}
		for _, entry := range entries {
			role, err := roleName(entry)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			pending = append(pending, role)
		}
		for _, section := range []string{"pre_tasks", "tasks", "post_tasks", "handlers"} {
			roles, err := taskRoles(play[section], false)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			pending = append(pending, roles...)
		}
	}
	closure := map[string]bool{}
	for len(pending) > 0 {
		role := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if closure[role] {
			continue
		}
		closure[role] = true
		references, err := roleReferences(ansible, role)
		if err != nil {
			return nil, err
		}
		pending = append(pending, references...)
	}
	return closure, nil
}

func roleName(entry any) (string, error) {
	name := entry
	if options, ok := entry.(map[string]any); ok {
		name = options["role"]
		if name == nil {
			name = options["name"]
		}
	}
	role, ok := name.(string)
	if !ok || !roleNamePattern.MatchString(role) {
		return "", fmt.Errorf("unsupported role reference %v", entry)
	}
	return role, nil
}

func taskRoles(value any, inRole bool) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	tasks, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("unsupported task list %v", value)
	}
	var roles []string
	for _, item := range tasks {
		task, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unsupported task %v", item)
		}
		for key, argument := range task {
			switch strings.TrimPrefix(key, "ansible.builtin.") {
			case "block", "rescue", "always":
				nested, err := taskRoles(argument, inRole)
				if err != nil {
					return nil, err
				}
				roles = append(roles, nested...)
			case "import_role", "include_role":
				role, err := roleName(argument)
				if err != nil {
					return nil, err
				}
				roles = append(roles, role)
			case "import_tasks", "include_tasks":
				file := argument
				if options, ok := argument.(map[string]any); ok {
					file = options["file"]
				}
				name, ok := file.(string)
				if !inRole || !ok || filepath.IsAbs(name) || strings.Contains(name, "..") || strings.Contains(name, "{{") {
					return nil, fmt.Errorf("unsupported task include %v", argument)
				}
			}
		}
	}
	return roles, nil
}

func roleReferences(ansible, role string) ([]string, error) {
	directory := filepath.Join(ansible, "roles", role)
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("role %s is not declared", role)
	}
	var references []string
	meta, err := os.ReadFile(filepath.Join(directory, "meta", "main.yml"))
	switch {
	case err == nil:
		var declaration struct{ Dependencies []any }
		if err := yaml.Unmarshal(meta, &declaration); err != nil {
			return nil, fmt.Errorf("role %s metadata: %w", role, err)
		}
		for _, dependency := range declaration.Dependencies {
			name, err := roleName(dependency)
			if err != nil {
				return nil, fmt.Errorf("role %s: %w", role, err)
			}
			references = append(references, name)
		}
	case !os.IsNotExist(err):
		return nil, err
	}
	for _, section := range []string{"tasks", "handlers"} {
		err := filepath.WalkDir(filepath.Join(directory, section), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || (filepath.Ext(path) != ".yml" && filepath.Ext(path) != ".yaml") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var tasks any
			if err := yaml.Unmarshal(data, &tasks); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			roles, err := taskRoles(tasks, true)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			references = append(references, roles...)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return references, nil
}
