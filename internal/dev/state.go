package dev

import "path/filepath"

type State struct {
	Root  string
	Cache string
}

func NewState(root string) State {
	return State{Root: root, Cache: filepath.Join(root, ".cache", "dev")}
}

func (s State) Tools() string   { return filepath.Join(s.Cache, "tools") }
func (s State) Reports() string { return filepath.Join(s.Cache, "reports") }
func (s State) Render() string  { return filepath.Join(s.Cache, "render") }
func (s State) Venv() string    { return filepath.Join(s.Root, ".venv") }
